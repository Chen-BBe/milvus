// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package utils

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/multierr"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/querycoordv2/meta"
	"github.com/milvus-io/milvus/internal/querycoordv2/session"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/proto/querypb"
	"github.com/milvus-io/milvus/pkg/v2/util/merr"
	"github.com/milvus-io/milvus/pkg/v2/util/paramtable"
)

func CheckNodeAvailable(nodeID int64, info *session.NodeInfo) error {
	if info == nil {
		return merr.WrapErrNodeOffline(nodeID)
	}
	return nil
}

// In a replica, a shard is available, if and only if:
// 1. The leader is online
// 2. All QueryNodes in the distribution are online
// 3. The last heartbeat response time is within HeartbeatAvailableInterval for all QueryNodes(include leader) in the distribution
// 4. All segments of the shard in target should be in the distribution
func CheckDelegatorDataReady(nodeMgr *session.NodeManager, targetMgr meta.TargetManagerInterface, leader *meta.LeaderView, scope int32) error {
	log := log.Ctx(context.TODO()).
		WithRateGroup(fmt.Sprintf("util.CheckDelegatorDataReady-%d", leader.CollectionID), 1, 60).
		With(zap.Int64("leaderID", leader.ID), zap.Int64("collectionID", leader.CollectionID))

	// Check whether leader is online
	info := nodeMgr.Get(leader.ID)
	if info == nil {
		err := merr.WrapErrNodeOffline(leader.ID)
		log.Info("leader is not available", zap.Error(err))
		return fmt.Errorf("leader not available: %w", err)
	}

	segmentDist := targetMgr.GetSealedSegmentsByChannel(context.TODO(), leader.CollectionID, leader.Channel, scope)
	// Check whether segments are fully loaded
	for segmentID := range segmentDist {
		version, exist := leader.Segments[segmentID]
		if !exist {
			log.RatedInfo(10, "leader is not available due to lack of segment", zap.Int64("segmentID", segmentID))
			return merr.WrapErrSegmentLack(segmentID)
		}

		// Check whether segment's worker node is online
		info := nodeMgr.Get(version.GetNodeID())
		if info == nil {
			err := merr.WrapErrNodeOffline(leader.ID)
			log.Info("leader is not available due to QueryNode unavailable",
				zap.Int64("segmentID", segmentID),
				zap.Error(err))
			return err
		}
	}
	return nil
}

func checkLoadStatus(ctx context.Context, m *meta.Meta, collectionID int64) error {
	percentage := m.CollectionManager.CalculateLoadPercentage(ctx, collectionID)
	if percentage < 0 {
		err := merr.WrapErrCollectionNotLoaded(collectionID)
		log.Ctx(ctx).Warn("failed to GetShardLeaders", zap.Error(err))
		return err
	}
	collection := m.CollectionManager.GetCollection(ctx, collectionID)
	if collection != nil && collection.GetStatus() == querypb.LoadStatus_Loaded {
		// when collection is loaded, regard collection as readable, set percentage == 100
		percentage = 100
	}

	if percentage < 100 {
		err := merr.WrapErrCollectionNotFullyLoaded(collectionID)
		msg := fmt.Sprintf("collection %v is not fully loaded", collectionID)
		log.Ctx(ctx).Warn(msg)
		return err
	}
	return nil
}

func GetShardLeadersWithChannels(ctx context.Context, m *meta.Meta, targetMgr meta.TargetManagerInterface, dist *meta.DistributionManager,
	nodeMgr *session.NodeManager, collectionID int64, channels map[string]*meta.DmChannel,
) ([]*querypb.ShardLeadersList, error) {
	ret := make([]*querypb.ShardLeadersList, 0)
	for _, channel := range channels {
		log := log.Ctx(ctx).With(zap.String("channel", channel.GetChannelName()))

		var channelErr error
		leaders := dist.LeaderViewManager.GetByFilter(meta.WithChannelName2LeaderView(channel.GetChannelName()))
		if len(leaders) == 0 {
			channelErr = merr.WrapErrChannelLack(channel.GetChannelName(), "channel not subscribed")
		}

		readableLeaders := make(map[int64]*meta.LeaderView)
		for _, leader := range leaders {
			if leader.UnServiceableError != nil {
				multierr.AppendInto(&channelErr, leader.UnServiceableError)
				continue
			}
			readableLeaders[leader.ID] = leader
		}

		if len(readableLeaders) == 0 {
			msg := fmt.Sprintf("channel %s is not available in any replica", channel.GetChannelName())
			log.Warn(msg, zap.Error(channelErr))
			err := merr.WrapErrChannelNotAvailable(channel.GetChannelName(), channelErr.Error())
			return nil, err
		}

		readableLeaders = filterDupLeaders(ctx, m.ReplicaManager, readableLeaders)
		ids := make([]int64, 0, len(leaders))
		addrs := make([]string, 0, len(leaders))
		for _, leader := range readableLeaders {
			info := nodeMgr.Get(leader.ID)
			if info != nil {
				ids = append(ids, info.ID())
				addrs = append(addrs, info.Addr())
			}
		}

		// to avoid node down during GetShardLeaders
		if len(ids) == 0 {
			if channelErr == nil {
				channelErr = merr.WrapErrChannelNotAvailable(channel.GetChannelName())
			}
			msg := fmt.Sprintf("channel %s is not available in any replica", channel.GetChannelName())
			log.Warn(msg, zap.Error(channelErr))
			err := merr.WrapErrChannelNotAvailable(channel.GetChannelName(), channelErr.Error())
			return nil, err
		}

		ret = append(ret, &querypb.ShardLeadersList{
			ChannelName: channel.GetChannelName(),
			NodeIds:     ids,
			NodeAddrs:   addrs,
		})
	}

	return ret, nil
}

func GetShardLeaders(ctx context.Context, m *meta.Meta, targetMgr meta.TargetManagerInterface, dist *meta.DistributionManager, nodeMgr *session.NodeManager, collectionID int64) ([]*querypb.ShardLeadersList, error) {
	if err := checkLoadStatus(ctx, m, collectionID); err != nil {
		return nil, err
	}

	channels := targetMgr.GetDmChannelsByCollection(ctx, collectionID, meta.CurrentTarget)
	if len(channels) == 0 {
		msg := "loaded collection do not found any channel in target, may be in recovery"
		err := merr.WrapErrCollectionOnRecovering(collectionID, msg)
		log.Ctx(ctx).Warn("failed to get channels", zap.Error(err))
		return nil, err
	}
	return GetShardLeadersWithChannels(ctx, m, targetMgr, dist, nodeMgr, collectionID, channels)
}

// CheckCollectionsQueryable check all channels are watched and all segments are loaded for this collection
func CheckCollectionsQueryable(ctx context.Context, m *meta.Meta, targetMgr meta.TargetManagerInterface, dist *meta.DistributionManager, nodeMgr *session.NodeManager) error {
	maxInterval := paramtable.Get().QueryCoordCfg.UpdateCollectionLoadStatusInterval.GetAsDuration(time.Minute)
	for _, coll := range m.GetAllCollections(ctx) {
		err := checkCollectionQueryable(ctx, m, targetMgr, dist, nodeMgr, coll)
		// the collection is not queryable, if meet following conditions:
		// 1. Some segments are not loaded
		// 2. Collection is not starting to release
		// 3. The load percentage has not been updated in the last 5 minutes.
		if err != nil && m.Exist(ctx, coll.CollectionID) && time.Since(coll.UpdatedAt) >= maxInterval {
			log.Ctx(ctx).Warn("collection not querable",
				zap.Int64("collectionID", coll.CollectionID),
				zap.Time("lastUpdated", coll.UpdatedAt),
				zap.Duration("maxInterval", maxInterval),
				zap.Error(err))
			return err
		}
	}
	return nil
}

// checkCollectionQueryable check all channels are watched and all segments are loaded for this collection
func checkCollectionQueryable(ctx context.Context, m *meta.Meta, targetMgr meta.TargetManagerInterface, dist *meta.DistributionManager, nodeMgr *session.NodeManager, coll *meta.Collection) error {
	collectionID := coll.GetCollectionID()
	if err := checkLoadStatus(ctx, m, collectionID); err != nil {
		return err
	}

	channels := targetMgr.GetDmChannelsByCollection(ctx, collectionID, meta.CurrentTarget)
	if len(channels) == 0 {
		msg := "loaded collection do not found any channel in target, may be in recovery"
		err := merr.WrapErrCollectionOnRecovering(collectionID, msg)
		log.Ctx(ctx).Warn("failed to get channels", zap.Error(err))
		return err
	}

	shardList, err := GetShardLeadersWithChannels(ctx, m, targetMgr, dist, nodeMgr, collectionID, channels)
	if err != nil {
		return err
	}

	if len(channels) != len(shardList) {
		return merr.WrapErrCollectionNotFullyLoaded(collectionID, "still have unwatched channels or loaded segments")
	}

	return nil
}

func filterDupLeaders(ctx context.Context, replicaManager *meta.ReplicaManager, leaders map[int64]*meta.LeaderView) map[int64]*meta.LeaderView {
	type leaderID struct {
		ReplicaID int64
		Shard     string
	}

	newLeaders := make(map[leaderID]*meta.LeaderView)
	for _, view := range leaders {
		replica := replicaManager.GetByCollectionAndNode(ctx, view.CollectionID, view.ID)
		if replica == nil {
			continue
		}

		id := leaderID{replica.GetID(), view.Channel}
		if old, ok := newLeaders[id]; ok && old.Version > view.Version {
			continue
		}

		newLeaders[id] = view
	}

	result := make(map[int64]*meta.LeaderView)
	for _, v := range newLeaders {
		result[v.ID] = v
	}
	return result
}
