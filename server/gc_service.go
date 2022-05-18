// Copyright 2022 TiKV Project Authors.
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

package server

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/pingcap/kvproto/pkg/gcpb"
	"github.com/pingcap/log"
	"github.com/tikv/pd/pkg/tsoutil"
	"github.com/tikv/pd/server/storage/endpoint"
	"github.com/tikv/pd/server/tso"
	"go.uber.org/zap"
)

// GcServer wraps Server to provide garbage collection service.
type GcServer struct {
	*Server
}

func (s *GcServer) header() *gcpb.ResponseHeader {
	return &gcpb.ResponseHeader{ClusterId: s.clusterID}
}

func (s *GcServer) errorHeader(err *gcpb.Error) *gcpb.ResponseHeader {
	return &gcpb.ResponseHeader{
		ClusterId: s.clusterID,
		Error:     err,
	}
}

func (s *GcServer) notBootstrappedHeader() *gcpb.ResponseHeader {
	return s.errorHeader(&gcpb.Error{
		Type:    gcpb.ErrorType_NOT_BOOTSTRAPPED,
		Message: "cluster is not bootstrapped",
	})
}

func (s *GcServer) revisionMismatchHeader(requestRevision, currentRevision int64) *gcpb.ResponseHeader {
	return s.errorHeader(&gcpb.Error{
		Type:    gcpb.ErrorType_REVISION_MISMATCH,
		Message: fmt.Sprintf("revision mismatch, requested revision %v but current revision %v", requestRevision, currentRevision),
	})
}

func (s *GcServer) safePointRollbackHeader(requestSafePoint, requiredSafePoint uint64) *gcpb.ResponseHeader {
	return s.errorHeader(&gcpb.Error{
		Type:    gcpb.ErrorType_SAFEPOINT_ROLLBACK,
		Message: fmt.Sprintf("safe point rollback, requested safe point %v is less than required safe point %v", requestSafePoint, requiredSafePoint),
	})
}

// ListKeySpaces returns all key spaces that has gc safe point.
// If withGCSafePoint set to true, it will also return their corresponding gc safe points, otherwise they will be 0.
func (s *GcServer) ListKeySpaces(ctx context.Context, request *gcpb.ListKeySpacesRequest) (*gcpb.ListKeySpacesResponse, error) {
	rc := s.GetRaftCluster()
	if rc == nil {
		return &gcpb.ListKeySpacesResponse{Header: s.notBootstrappedHeader()}, nil
	}

	var storage endpoint.KeySpaceGCSafePointStorage = s.storage
	keySpaces, err := storage.LoadAllKeySpaceGCSafePoints(request.WithGcSafePoint)
	if err != nil {
		return nil, err
	}

	returnKeySpaces := make([]*gcpb.KeySpace, 0, len(keySpaces))
	for _, keySpace := range keySpaces {
		returnKeySpaces = append(returnKeySpaces, &gcpb.KeySpace{
			SpaceId:     []byte(keySpace.SpaceID),
			GcSafePoint: keySpace.SafePoint,
		})
	}

	return &gcpb.ListKeySpacesResponse{
		Header:    s.header(),
		KeySpaces: returnKeySpaces,
	}, nil
}

// getKeySpaceRevision return etcd ModRevision of given key space.
// It's used to detect new service safe point between `GetMinServiceSafePoint` & `UpdateServiceSafePoint`.
// Return `kv.RevisionUnavailable` if the service group is not existed.
func (s *GcServer) getKeySpaceRevision(spaceID string) (int64, error) {
	keySpacePath := endpoint.KeySpacePath(spaceID)
	_, revision, err := s.storage.LoadRevision(keySpacePath)
	return revision, err
}

// touchKeySpaceRevision advances revision of given key space.
// It's used when new service safe point is saved.
func (s *GcServer) touchKeySpaceRevision(spaceID string) error {
	keySpacePath := endpoint.KeySpacePath(spaceID)
	return s.storage.Save(keySpacePath, "")
}

func (s *GcServer) getNow() (time.Time, error) {
	nowTSO, err := s.tsoAllocatorManager.HandleTSORequest(tso.GlobalDCLocation, 1)
	if err != nil {
		return time.Time{}, err
	}
	now, _ := tsoutil.ParseTimestamp(nowTSO)
	return now, err
}

// GetMinServiceSafePoint returns given service group's min service safe point.
func (s *GcServer) GetMinServiceSafePoint(ctx context.Context, request *gcpb.GetMinServiceSafePointRequest) (*gcpb.GetMinServiceSafePointResponse, error) {
	// Lock to ensure that there is no other change between `min` and `currentRevision`.
	// Also note that `storage.LoadMinServiceSafePoint` is not thread-safe.
	s.keySpaceGCLock.Lock()
	defer s.keySpaceGCLock.Unlock()

	rc := s.GetRaftCluster()
	if rc == nil {
		return &gcpb.GetMinServiceSafePointResponse{Header: s.notBootstrappedHeader()}, nil
	}

	var storage endpoint.KeySpaceGCSafePointStorage = s.storage
	requestSpaceID := string(request.GetSpaceId())

	now, err := s.getNow()
	if err != nil {
		return nil, err
	}

	min, err := storage.LoadMinServiceSafePoint(requestSpaceID, now)
	if err != nil {
		return nil, err
	}
	var returnSafePoint uint64
	if min != nil {
		returnSafePoint = min.SafePoint
	}

	currentRevision, err := s.getKeySpaceRevision(requestSpaceID)
	if err != nil {
		return nil, err
	}

	return &gcpb.GetMinServiceSafePointResponse{
		Header:    s.header(),
		SafePoint: returnSafePoint,
		Revision:  currentRevision,
	}, nil
}

// UpdateGCSafePoint used by gc_worker to update their gc safe points.
func (s *GcServer) UpdateGCSafePoint(ctx context.Context, request *gcpb.UpdateGCSafePointRequest) (*gcpb.UpdateGCSafePointResponse, error) {
	s.keySpaceGCLock.Lock()
	defer s.keySpaceGCLock.Unlock()

	rc := s.GetRaftCluster()
	if rc == nil {
		return &gcpb.UpdateGCSafePointResponse{Header: s.notBootstrappedHeader()}, nil
	}

	var storage endpoint.KeySpaceGCSafePointStorage = s.storage
	requestSpaceID := string(request.GetSpaceId())
	requestSafePoint := request.GetSafePoint()
	requestRevision := request.GetRevision()

	// check if revision changed since last min calculation.
	currentRevision, err := s.getKeySpaceRevision(requestSpaceID)
	if err != nil {
		return nil, err
	}
	if currentRevision != requestRevision {
		return &gcpb.UpdateGCSafePointResponse{
			Header:       s.revisionMismatchHeader(requestRevision, currentRevision),
			Succeeded:    false,
			NewSafePoint: 0,
		}, nil
	}

	oldSafePoint, err := storage.LoadKeySpaceGCSafePoint(requestSpaceID)
	if err != nil {
		return nil, err
	}
	response := &gcpb.UpdateGCSafePointResponse{}

	// fail to store due to safe point rollback.
	if requestSafePoint < oldSafePoint {
		log.Warn("trying to update gc_worker safe point",
			zap.String("key-space", requestSpaceID),
			zap.Uint64("old-safe-point", oldSafePoint),
			zap.Uint64("new-safe-point", requestSafePoint))
		response.Header = s.safePointRollbackHeader(requestSafePoint, oldSafePoint)
		response.Succeeded = false
		response.NewSafePoint = oldSafePoint
		return response, nil
	}

	// save the safe point to storage.
	if err := storage.SaveKeySpaceGCSafePoint(requestSpaceID, requestSafePoint); err != nil {
		return nil, err
	}
	response.Header = s.header()
	response.Succeeded = true
	response.NewSafePoint = requestSafePoint
	log.Info("updated gc_worker safe point",
		zap.String("key-space", requestSpaceID),
		zap.Uint64("old-safe-point", oldSafePoint),
		zap.Uint64("new-safe-point", requestSafePoint))
	return response, nil
}

// UpdateServiceSafePoint for services like CDC/BR/Lightning to update gc safe points in PD.
func (s *GcServer) UpdateServiceSafePoint(ctx context.Context, request *gcpb.UpdateServiceSafePointRequest) (*gcpb.UpdateServiceSafePointResponse, error) {
	s.keySpaceGCLock.Lock()
	defer s.keySpaceGCLock.Unlock()

	rc := s.GetRaftCluster()
	if rc == nil {
		return &gcpb.UpdateServiceSafePointResponse{Header: s.notBootstrappedHeader()}, nil
	}

	var storage endpoint.KeySpaceGCSafePointStorage = s.storage
	requestSpaceID := string(request.GetSpaceId())
	requestServiceID := string(request.GetServiceId())
	requestTTL := request.GetTTL()
	requestSafePoint := request.GetSafePoint()

	// a less than 0 ttl means to remove the safe point, immediately return after the deletion request.
	if requestTTL <= 0 {
		if err := storage.RemoveServiceSafePoint(requestSpaceID, requestServiceID); err != nil {
			return nil, err
		}
		return &gcpb.UpdateServiceSafePointResponse{
			Header:    s.header(),
			Succeeded: true,
		}, nil
	}

	now, err := s.getNow()
	if err != nil {
		return nil, err
	}

	oldServiceSafePoint, err := storage.LoadServiceSafePoint(requestSpaceID, requestServiceID)
	if err != nil {
		return nil, err
	}
	gcSafePoint, err := storage.LoadKeySpaceGCSafePoint(requestSpaceID)
	if err != nil {
		return nil, err
	}

	response := &gcpb.UpdateServiceSafePointResponse{GcSafePoint: gcSafePoint}
	// safePointLowerBound is the minimum request.SafePoint for update request to succeed.
	// It is oldServiceSafePoint if oldServiceSafePoint exists, else gcSafePoint if it exists.
	// For any new service, this will be 0, indicate any safePoint would be accepted.
	var safePointLowerBound uint64 = 0
	if oldServiceSafePoint != nil {
		safePointLowerBound = oldServiceSafePoint.SafePoint
		response.OldSafePoint = oldServiceSafePoint.SafePoint
	} else {
		safePointLowerBound = gcSafePoint
		response.OldSafePoint = 0
	}

	// If requestSafePoint is smaller than safePointLowerBound, we have a safePointRollBack.
	if requestSafePoint < safePointLowerBound {
		response.Header = s.safePointRollbackHeader(requestSafePoint, safePointLowerBound)
		response.Succeeded = false
		return response, nil
	}

	response.Succeeded = true
	response.NewSafePoint = requestSafePoint
	ssp := &endpoint.ServiceSafePoint{
		ServiceID: requestServiceID,
		ExpiredAt: now.Unix() + request.TTL,
		SafePoint: request.SafePoint,
	}
	// Handles overflow.
	if math.MaxInt64-now.Unix() <= request.TTL {
		ssp.ExpiredAt = math.MaxInt64
	}

	if oldServiceSafePoint == nil {
		// Touch service revision to advance revision, for indicating that a new service safe point is added.
		// Should be invoked before `SaveServiceSafePoint`, to avoid touch fail after new service safe point is saved.
		if err := s.touchKeySpaceRevision(requestSpaceID); err != nil {
			return nil, err
		}
	}

	if err := storage.SaveServiceSafePoint(requestSpaceID, ssp); err != nil {
		return nil, err
	}
	log.Info("updated service safe point",
		zap.String("key-space", requestSpaceID),
		zap.String("service-id", ssp.ServiceID),
		zap.Int64("expire-at", ssp.ExpiredAt),
		zap.Uint64("safepoint", ssp.SafePoint))
	return response, nil
}
