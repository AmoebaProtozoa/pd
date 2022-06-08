// Copyright 2016 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kv

import (
	"context"
	"path"
	"strings"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/log"
	"github.com/tikv/pd/pkg/errs"
	"github.com/tikv/pd/pkg/etcdutil"
	"go.etcd.io/etcd/clientv3"
	"go.uber.org/zap"
)

const (
	requestTimeout  = 10 * time.Second
	slowRequestTime = 1 * time.Second
	// RevisionUnavailable is the value of unavailable revision,
	// when the kv does not exist.
	RevisionUnavailable int64 = -1
)

// EtcdKVBase is a kv store using etcd.
type EtcdKVBase struct {
	client   *clientv3.Client
	rootPath string
}

// NewEtcdKVBase creates a new etcd kv.
func NewEtcdKVBase(client *clientv3.Client, rootPath string) *EtcdKVBase {
	return &EtcdKVBase{
		client:   client,
		rootPath: rootPath,
	}
}

// Load gets a value for a given key.
func (kv *EtcdKVBase) Load(key string) (string, error) {
	value, _, err := kv.LoadRevision(key)
	return value, err
}

// LoadRevision gets a value along with revision.
func (kv *EtcdKVBase) LoadRevision(key string) (string, int64, error) {
	key = path.Join(kv.rootPath, key)

	resp, err := etcdutil.EtcdKVGet(kv.client, key)
	if err != nil {
		return "", RevisionUnavailable, err
	}
	if n := len(resp.Kvs); n == 0 {
		return "", RevisionUnavailable, nil
	} else if n > 1 {
		return "", RevisionUnavailable, errs.ErrEtcdKVGetResponse.GenWithStackByArgs(resp.Kvs)
	}
	return string(resp.Kvs[0].Value), resp.Kvs[0].ModRevision, nil
}

// LoadRange gets a range of value for a given key range.
func (kv *EtcdKVBase) LoadRange(key, endKey string, limit int) ([]string, []string, error) {
	// Note: reason to use `strings.Join` instead of `path.Join` is that the latter will
	// removes suffix '/' of the joined string.
	// As a result, when we try to scan from "foo/", it ends up scanning from "/pd/foo"
	// internally, and returns unexpected keys such as "foo_bar/baz".
	key = strings.Join([]string{kv.rootPath, key}, "/")
	endKey = strings.Join([]string{kv.rootPath, endKey}, "/")

	withRange := clientv3.WithRange(endKey)
	withLimit := clientv3.WithLimit(int64(limit))
	resp, err := etcdutil.EtcdKVGet(kv.client, key, withRange, withLimit)
	if err != nil {
		return nil, nil, err
	}
	keys := make([]string, 0, len(resp.Kvs))
	values := make([]string, 0, len(resp.Kvs))
	for _, item := range resp.Kvs {
		keys = append(keys, strings.TrimPrefix(strings.TrimPrefix(string(item.Key), kv.rootPath), "/"))
		values = append(values, string(item.Value))
	}
	return keys, values, nil
}

// SaveWithTTL stores a key-value pair that expires after ttlSeconds seconds.
func (kv *EtcdKVBase) SaveWithTTL(key, value string, ttlSeconds int64) error {
	key = path.Join(kv.rootPath, key)
	start := time.Now()
	ctx, cancel := context.WithTimeout(kv.client.Ctx(), requestTimeout)
	resp, err := etcdutil.EtcdKVPutWithTTL(ctx, kv.client, key, value, ttlSeconds)
	cancel()

	cost := time.Since(start)
	if cost > slowRequestTime {
		log.Warn("save to etcd with lease runs too slow",
			zap.Reflect("response", resp),
			zap.Duration("cost", cost),
			errs.ZapError(err))
	}

	if err != nil {
		e := errs.ErrEtcdKVPut.Wrap(err).GenWithStackByCause()
		log.Error("save to etcd with lease meet error",
			zap.String("key", key),
			zap.String("value", value),
			zap.Int64("ttl-seconds", ttlSeconds),
			errs.ZapError(e),
		)
		return e
	}
	return nil
}

// Save stores a key-value pair.
func (kv *EtcdKVBase) Save(key, value string) error {
	failpoint.Inject("etcdSaveFailed", func() {
		failpoint.Return(errors.New("save failed"))
	})
	key = path.Join(kv.rootPath, key)
	txn := NewSlowLogTxn(kv.client)
	resp, err := txn.Then(clientv3.OpPut(key, value)).Commit()
	if err != nil {
		e := errs.ErrEtcdKVPut.Wrap(err).GenWithStackByCause()
		log.Error("save to etcd meet error", zap.String("key", key), zap.String("value", value), errs.ZapError(e))
		return e
	}
	if !resp.Succeeded {
		return errs.ErrEtcdTxnConflict.FastGenByArgs()
	}
	return nil
}

// Remove deletes a key-value pair for a given key.
func (kv *EtcdKVBase) Remove(key string) error {
	key = path.Join(kv.rootPath, key)

	txn := NewSlowLogTxn(kv.client)
	resp, err := txn.Then(clientv3.OpDelete(key)).Commit()
	if err != nil {
		err = errs.ErrEtcdKVDelete.Wrap(err).GenWithStackByCause()
		log.Error("remove from etcd meet error", zap.String("key", key), errs.ZapError(err))
		return err
	}
	if !resp.Succeeded {
		return errs.ErrEtcdTxnConflict.FastGenByArgs()
	}
	return nil
}

// SlowLogTxn wraps etcd transaction and log slow one.
type SlowLogTxn struct {
	clientv3.Txn
	cancel context.CancelFunc
}

// NewSlowLogTxn create a SlowLogTxn.
func NewSlowLogTxn(client *clientv3.Client) clientv3.Txn {
	ctx, cancel := context.WithTimeout(client.Ctx(), requestTimeout)
	return &SlowLogTxn{
		Txn:    client.Txn(ctx),
		cancel: cancel,
	}
}

// If takes a list of comparison. If all comparisons passed in succeed,
// the operations passed into Then() will be executed. Or the operations
// passed into Else() will be executed.
func (t *SlowLogTxn) If(cs ...clientv3.Cmp) clientv3.Txn {
	t.Txn = t.Txn.If(cs...)
	return t
}

// Then takes a list of operations. The Ops list will be executed, if the
// comparisons passed in If() succeed.
func (t *SlowLogTxn) Then(ops ...clientv3.Op) clientv3.Txn {
	t.Txn = t.Txn.Then(ops...)
	return t
}

// Commit implements Txn Commit interface.
func (t *SlowLogTxn) Commit() (*clientv3.TxnResponse, error) {
	start := time.Now()
	resp, err := t.Txn.Commit()
	t.cancel()

	cost := time.Since(start)
	if cost > slowRequestTime {
		log.Warn("txn runs too slow",
			zap.Reflect("response", resp),
			zap.Duration("cost", cost),
			errs.ZapError(err))
	}
	label := "success"
	if err != nil {
		label = "failed"
	}
	txnCounter.WithLabelValues(label).Inc()
	txnDuration.WithLabelValues(label).Observe(cost.Seconds())

	return resp, errors.WithStack(err)
}
