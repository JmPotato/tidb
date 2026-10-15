// Copyright 2025 PingCAP, Inc.
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

package affinity

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/pingcap/errors"

	"github.com/pingcap/tidb/pkg/util/logutil"
	pdhttp "github.com/tikv/pd/client/http"
	"go.uber.org/zap"
)

const (
	// maxRetryTimes is the max retry times for affinity group operations
	maxRetryTimes = 3
	// retryInterval is the sleep time before next retry
	retryInterval = 200 * time.Millisecond
)

var (
	manager  Manager
	pdClient pdhttp.Client
)

// InitManager initializes the package-level affinity manager and PD client.
// This is called by infosync during initialization.
func InitManager(pdCli pdhttp.Client) {
	if pdCli == nil {
		manager = NewMockManager()
	} else {
		manager = NewPDManager(pdCli)
	}
	pdClient = pdCli
}

// CreateGroupsIfNotExists creates affinity groups in PD (idempotent).
// It checks which groups already exist and only creates the ones that don't exist.
// This makes the operation safe for DDL job retries (e.g., after Owner switch or network failures).
func CreateGroupsIfNotExists(ctx context.Context, groups map[string][]pdhttp.AffinityGroupKeyRange) error {
	if len(groups) == 0 {
		return nil
	}
	return manager.CreateAffinityGroupsIfNotExists(ctx, groups)
}

// DeleteGroups removes affinity groups in PD with force=true.
func DeleteGroups(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return manager.DeleteAffinityGroups(ctx, ids)
}

// DeleteGroupsWithRetry deletes groups with retry logic.
// This is a best-effort cleanup operation. Errors are logged on final failure.
func DeleteGroupsWithRetry(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	var err error
	for i := 0; i <= maxRetryTimes; i++ {
		err = DeleteGroups(ctx, ids)
		if err == nil {
			return nil
		}
		if i != maxRetryTimes {
			time.Sleep(retryInterval)
		} else {
			// Only log error on final retry failure
			logutil.BgLogger().Error("Failed to delete affinity groups after retries", zap.Error(err), zap.Strings("groupIDs", ids))
		}
	}
	return err
}

// GetGroups gets affinity groups by their IDs.
func GetGroups(ctx context.Context, ids []string) (map[string]*pdhttp.AffinityGroupState, error) {
	if len(ids) == 0 {
		return make(map[string]*pdhttp.AffinityGroupState), nil
	}
	return manager.GetAffinityGroups(ctx, ids)
}

// UpdateGroupPeers sets the leader and voter stores of an affinity group.
func UpdateGroupPeers(ctx context.Context, id string, leaderStoreID uint64, voterStoreIDs []uint64) error {
	return manager.UpdateAffinityGroupPeers(ctx, id, leaderStoreID, voterStoreIDs)
}

// ListUpStoreIDs returns the IDs of the TiKV stores that are Up, in ascending
// order. It returns nil when there is no PD client, for example in unit tests
// with a mock store.
func ListUpStoreIDs(ctx context.Context) ([]uint64, error) {
	if pdClient == nil {
		return nil, nil
	}
	stores, err := pdClient.GetStores(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]uint64, 0, len(stores.Stores))
	for _, s := range stores.Stores {
		if s.Store.StateName != "Up" || isTiFlashStore(s.Store.Labels) {
			continue
		}
		ids = append(ids, uint64(s.Store.ID))
	}
	slices.Sort(ids)
	return ids, nil
}

func isTiFlashStore(labels []pdhttp.StoreLabel) bool {
	for _, l := range labels {
		if l.Key == "engine" && (l.Value == "tiflash" || l.Value == "tiflash_compute") {
			return true
		}
	}
	return false
}

// GetAffinityScheduleLimit returns PD schedule.affinity-schedule-limit. ok is
// false when there is no PD client or the value cannot be read.
func GetAffinityScheduleLimit(ctx context.Context) (limit uint64, ok bool) {
	if pdClient == nil {
		return 0, false
	}
	cfg, err := pdClient.GetScheduleConfig(ctx)
	if err != nil {
		return 0, false
	}
	v, ok := cfg["affinity-schedule-limit"].(float64)
	if !ok {
		return 0, false
	}
	return uint64(v), true
}

// GetRegionsInRange returns the Regions that overlap [startKey, endKey), whose
// keys are already encoded for PD. It returns nil when there is no PD client.
func GetRegionsInRange(ctx context.Context, startKey, endKey []byte) ([]pdhttp.RegionInfo, error) {
	if pdClient == nil {
		return nil, nil
	}
	regions, err := pdClient.GetRegionsByKeyRange(ctx, pdhttp.NewKeyRange(startKey, endKey), -1)
	if err != nil {
		return nil, err
	}
	return regions.Regions, nil
}

// GetGroupLeaderSources returns the leader_source reported by PD for each
// group: "explicit" when the target was set through the API, "fallback" when
// PD derived it from the first observed Region, and "" when PD has no target
// or does not report the field. The vendored PD HTTP client predates the
// field, so the response body is decoded here.
func GetGroupLeaderSources(ctx context.Context, ids []string) (map[string]string, error) {
	if pdClient == nil || len(ids) == 0 {
		return map[string]string{}, nil
	}
	var raw struct {
		AffinityGroups map[string]struct {
			LeaderSource string `json:"leader_source"`
		} `json:"affinity_groups"`
	}
	cli := pdClient.WithRespHandler(func(resp *http.Response, _ any) error {
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return errors.Errorf("get affinity groups: status %d: %s", resp.StatusCode, body)
		}
		return json.Unmarshal(body, &raw)
	})
	if _, err := cli.GetAllAffinityGroups(ctx); err != nil {
		return nil, err
	}
	sources := make(map[string]string, len(ids))
	for _, id := range ids {
		sources[id] = raw.AffinityGroups[id].LeaderSource
	}
	return sources, nil
}

// GetAllGroupStates gets all affinity group states from PD.
// This is used by SHOW AFFINITY to directly query PD without going through the manager.
func GetAllGroupStates(ctx context.Context) (map[string]*pdhttp.AffinityGroupState, error) {
	if pdClient == nil {
		return make(map[string]*pdhttp.AffinityGroupState), nil
	}
	return pdClient.GetAllAffinityGroups(ctx)
}

// SetPDClientForTest sets the PD client for testing.
// Please do not use it in the production environment.
func SetPDClientForTest(cli pdhttp.Client) func() {
	originalCli := pdClient
	pdClient = cli
	return func() {
		pdClient = originalCli
	}
}
