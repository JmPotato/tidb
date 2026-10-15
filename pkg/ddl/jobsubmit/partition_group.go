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

package jobsubmit

import (
	"fmt"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/meta"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/util/dbterror"
)

// assignPartitionGroupIDs assigns the IDs that come from partition groups. It
// runs in the job submission transaction after assignGIDsForJobs, which holds
// the global ID lock, so member slots are allocated one submitter at a time.
//
//   - CREATE PARTITION GROUP reserves PartitionNum*SlotSize contiguous IDs.
//   - CREATE TABLE of a member and TRUNCATE TABLE/PARTITION of a member take a
//     new slot of the group and replace the partition IDs assigned by
//     assignGIDsForJobs with IDs from the group's ranges. The replaced IDs are
//     simply left unused.
func assignPartitionGroupIDs(m *meta.Mutator, specs []*JobSpec) error {
	groups := make(map[int64]*model.PartitionGroupInfo)
	getGroup := func(ref *model.PartitionGroupRefInfo) (*model.PartitionGroupInfo, error) {
		if g, ok := groups[ref.ID]; ok {
			return g, nil
		}
		g, err := m.GetPartitionGroup(ref.ID)
		if err != nil || g.State != model.StatePublic {
			return nil, dbterror.ErrPartitionGroupNotExists.GenWithStackByArgs(ref.Name.O)
		}
		groups[ref.ID] = g
		return g, nil
	}
	takeSlot := func(g *model.PartitionGroupInfo) (uint64, error) {
		if g.RemainingSlots() == 0 {
			return 0, dbterror.ErrInvalidPartitionGroup.GenWithStackByArgs(fmt.Sprintf(
				"partition group %s has no member slots left (MAX_MEMBERS = %d; every member table, TRUNCATE TABLE and TRUNCATE PARTITION takes one)",
				g.Name.O, g.SlotSize))
		}
		slot := g.NextSlot
		g.NextSlot++
		return slot, nil
	}
	assignMember := func(tbl *model.TableInfo) error {
		if tbl.PartitionGroup == nil {
			return nil
		}
		g, err := getGroup(tbl.PartitionGroup)
		if err != nil {
			return err
		}
		slot, err := takeSlot(g)
		if err != nil {
			return err
		}
		defs := tbl.GetPartitionInfo().Definitions
		for i := range defs {
			defs[i].ID = g.PhysicalID(uint64(i), slot)
		}
		return nil
	}

	for _, spec := range specs {
		if spec.IDAllocated {
			continue
		}
		switch spec.Job.Type {
		case model.ActionCreatePartitionGroup:
			g := spec.Args.(*model.PartitionGroupArgs).PGInfo
			last, err := m.AdvanceGlobalIDs(int(g.PartitionNum * g.SlotSize))
			if err != nil {
				return errors.Trace(err)
			}
			g.BaseID = last + 1
		case model.ActionCreateTable:
			if err := assignMember(spec.Args.(*model.CreateTableArgs).TableInfo); err != nil {
				return err
			}
		case model.ActionCreateTables:
			for _, tblArgs := range spec.Args.(*model.BatchCreateTableArgs).Tables {
				if err := assignMember(tblArgs.TableInfo); err != nil {
					return err
				}
			}
		case model.ActionTruncateTable, model.ActionTruncateTablePartition:
			tbl, err := m.GetTable(spec.Job.SchemaID, spec.Job.TableID)
			if err != nil {
				return errors.Trace(err)
			}
			if tbl == nil || tbl.PartitionGroup == nil {
				continue
			}
			g, err := getGroup(tbl.PartitionGroup)
			if err != nil {
				return err
			}
			slot, err := takeSlot(g)
			if err != nil {
				return err
			}
			args := spec.Args.(*model.TruncateTableArgs)
			for i, oldID := range args.OldPartitionIDs {
				offset := oldID - g.BaseID
				if offset < 0 || uint64(offset) >= g.PartitionNum*g.SlotSize {
					return errors.Errorf("partition %d of table %s is outside of partition group %s", oldID, tbl.Name.O, g.Name.O)
				}
				args.NewPartitionIDs[i] = g.PhysicalID(uint64(offset)/g.SlotSize, slot)
			}
		}
	}

	for _, g := range groups {
		if err := m.UpdatePartitionGroup(g); err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}
