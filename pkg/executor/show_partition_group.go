// Copyright 2026 PingCAP, Inc.
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

package executor

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pingcap/tidb/pkg/ddl"
	"github.com/pingcap/tidb/pkg/domain"
	"github.com/pingcap/tidb/pkg/domain/affinity"
	infoschemacontext "github.com/pingcap/tidb/pkg/infoschema/context"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/privilege"
	"github.com/pingcap/tidb/pkg/util/dbterror"
	pdhttp "github.com/tikv/pd/client/http"
)

// partitionGroupMembers returns the qualified names of the current member
// tables of each partition group that the user can see, sorted by name.
func (e *ShowExec) partitionGroupMembers() map[int64][]string {
	is := domain.GetDomain(e.Ctx()).InfoSchema()
	checker := privilege.GetPrivilegeManager(e.Ctx())
	activeRoles := e.Ctx().GetSessionVars().ActiveRoles
	members := make(map[int64][]string)
	for _, result := range is.ListTablesWithSpecialAttribute(infoschemacontext.PartitionGroupAttribute) {
		for _, tbl := range result.TableInfos {
			if checker != nil && !checker.RequestVerification(activeRoles, result.DBName.O, tbl.Name.O, "", mysql.AllPrivMask) {
				continue
			}
			members[tbl.PartitionGroup.ID] = append(members[tbl.PartitionGroup.ID], result.DBName.O+"."+tbl.Name.O)
		}
	}
	for _, names := range members {
		slices.Sort(names)
	}
	return members
}

func (e *ShowExec) fetchShowPartitionGroups() error {
	groups, err := ddl.ListPartitionGroups(e.Ctx().GetStore())
	if err != nil {
		return err
	}
	slices.SortFunc(groups, func(a, b *model.PartitionGroupInfo) int { return strings.Compare(a.Name.L, b.Name.L) })
	members := e.partitionGroupMembers()

	var fieldFilter string
	if e.Extractor != nil {
		fieldFilter = e.Extractor.Field()
	}
	for _, g := range groups {
		if fieldFilter != "" && g.Name.L != fieldFilter {
			continue
		}
		if e.Extractor != nil && e.Extractor.FieldPatternLike() != nil && !e.Extractor.FieldPatternLike().DoMatch(g.Name.L) {
			continue
		}
		names := members[g.ID]
		e.appendRow([]any{
			g.Name.O,
			g.ID,
			g.State.String(),
			g.PartitionNum,
			g.SlotSize,
			g.RemainingSlots(),
			len(names),
			strings.Join(names, ","),
		})
	}
	return nil
}

// fetchShowPartitionGroupStatus shows, for every partition index, the PD
// affinity group state and the Regions that currently cover the reserved key
// range. Single_region is YES only when exactly one Region covers the whole
// range and nothing else; it describes the layout, not the commit protocol of
// any transaction.
func (e *ShowExec) fetchShowPartitionGroupStatus(ctx context.Context) error {
	groups, err := ddl.ListPartitionGroups(e.Ctx().GetStore())
	if err != nil {
		return err
	}
	var g *model.PartitionGroupInfo
	for _, cand := range groups {
		if cand.Name.L == e.PartitionGroupName.L {
			g = cand
		}
	}
	if g == nil {
		return dbterror.ErrPartitionGroupNotExists.GenWithStackByArgs(e.PartitionGroupName.O)
	}

	states, stateErr := affinity.GetGroups(ctx, g.AffinityGroupIDs)
	sources, sourceErr := affinity.GetGroupLeaderSources(ctx, g.AffinityGroupIDs)
	codec := e.Ctx().GetStore().GetCodec()
	for i, groupID := range g.AffinityGroupIDs {
		idx := uint64(i)
		var errs []string
		if stateErr != nil {
			errs = append(errs, "affinity group: "+stateErr.Error())
		}
		if sourceErr != nil {
			errs = append(errs, "leader source: "+sourceErr.Error())
		}
		var (
			phase           string
			leader          uint64
			voters          string
			regionCount     int
			affRegionCount  int
			regionIDs       []string
			regionLeaders   []string
			singleRegion    = "NO"
			observedAt      = time.Now().UTC().Format(time.RFC3339)
			keyRange        = ddl.PartitionGroupAffinityKeyRange(codec, g, idx)
			state, hasState = states[groupID]
		)
		if hasState {
			phase = state.Phase
			leader = state.LeaderStoreID
			voters = joinUint64s(state.VoterStoreIDs)
			regionCount = state.RegionCount
			affRegionCount = state.AffinityRegionCount
		} else if stateErr == nil {
			errs = append(errs, "affinity group not found in PD")
		}

		regions, err := affinity.GetRegionsInRange(ctx, keyRange.StartKey, keyRange.EndKey)
		if err != nil {
			errs = append(errs, "regions: "+err.Error())
		}
		for _, r := range regions {
			regionIDs = append(regionIDs, fmt.Sprintf("%d(v%d)", r.ID, r.Epoch.Version))
			regionLeaders = append(regionLeaders, fmt.Sprintf("%d", r.Leader.StoreID))
		}
		if err == nil && len(regions) == 1 && regionCoversExactly(regions[0], keyRange) {
			singleRegion = "YES"
		}
		e.appendRow([]any{
			idx,
			g.RangeStart(idx),
			g.RangeEnd(idx),
			groupID,
			phase,
			leader,
			voters,
			leaderSource(sources[groupID]),
			regionCount,
			affRegionCount,
			strings.Join(regionIDs, ","),
			strings.Join(regionLeaders, ","),
			singleRegion,
			observedAt,
			strings.Join(errs, "; "),
		})
	}
	return nil
}

func regionCoversExactly(r pdhttp.RegionInfo, keyRange pdhttp.AffinityGroupKeyRange) bool {
	return strings.EqualFold(r.StartKey, hex.EncodeToString(keyRange.StartKey)) &&
		strings.EqualFold(r.EndKey, hex.EncodeToString(keyRange.EndKey))
}

func joinUint64s(vs []uint64) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = fmt.Sprintf("%d", v)
	}
	return strings.Join(parts, ",")
}

// leaderSource shows a group without a leader target as pending.
func leaderSource(source string) string {
	if source == "" {
		return "pending"
	}
	return source
}
