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

package model

import (
	"github.com/pingcap/tidb/pkg/parser/ast"
)

// DefaultPartitionGroupMaxMembers is the default number of member slots of a
// partition group when CREATE PARTITION GROUP does not specify MAX_MEMBERS.
const DefaultPartitionGroupMaxMembers = 1024

// MaxPartitionGroupMaxMembers bounds MAX_MEMBERS so that the reserved ID range
// of a group stays small relative to the global ID space.
const MaxPartitionGroupMaxMembers = 1 << 16

// PartitionGroupInfo is the metadata of a partition group.
//
// A partition group reserves PartitionNum contiguous physical ID ranges of
// SlotSize IDs each. Range i is [BaseID+i*SlotSize, BaseID+(i+1)*SlotSize).
// Every member table partitioned into PartitionNum HASH partitions takes one
// slot s, and its partition i gets the physical ID BaseID+i*SlotSize+s, so the
// partition i of all members is adjacent in the key space. PD schedules each
// range as one affinity group, whose ID is persisted in AffinityGroupIDs.
//
// Member tables are not recorded here; they reference the group through
// TableInfo.PartitionGroup.
type PartitionGroupInfo struct {
	ID           int64       `json:"id"`
	Name         ast.CIStr   `json:"name"`
	State        SchemaState `json:"state"`
	PartitionNum uint64      `json:"partition_num"`
	SlotSize     uint64      `json:"slot_size"`
	BaseID       int64       `json:"base_id"`
	// NextSlot is the next member slot to allocate. Slots are never reused:
	// creating a member table, TRUNCATE TABLE and TRUNCATE PARTITION each take
	// a new slot, including the ones of failed or rolled back jobs.
	NextSlot uint64 `json:"next_slot"`
	// AffinityGroupIDs[i] is the PD affinity group ID of range i.
	AffinityGroupIDs []string `json:"affinity_group_ids"`
}

// Clone clones PartitionGroupInfo.
func (g *PartitionGroupInfo) Clone() *PartitionGroupInfo {
	ng := *g
	ng.AffinityGroupIDs = append([]string(nil), g.AffinityGroupIDs...)
	return &ng
}

// RangeStart returns the first physical ID reserved for partition index idx.
func (g *PartitionGroupInfo) RangeStart(idx uint64) int64 {
	return g.BaseID + int64(idx*g.SlotSize)
}

// RangeEnd returns the end (exclusive) of the physical IDs reserved for
// partition index idx.
func (g *PartitionGroupInfo) RangeEnd(idx uint64) int64 {
	return g.RangeStart(idx + 1)
}

// PhysicalID returns the physical ID of partition index idx for the given slot.
func (g *PartitionGroupInfo) PhysicalID(idx, slot uint64) int64 {
	return g.RangeStart(idx) + int64(slot)
}

// RemainingSlots returns the number of member slots that can still be allocated.
func (g *PartitionGroupInfo) RemainingSlots() uint64 {
	if g.NextSlot >= g.SlotSize {
		return 0
	}
	return g.SlotSize - g.NextSlot
}

// PartitionGroupRefInfo references the partition group of a member table.
type PartitionGroupRefInfo struct {
	ID   int64     `json:"id"`
	Name ast.CIStr `json:"name"`
}

// Clone clones PartitionGroupRefInfo.
func (r *PartitionGroupRefInfo) Clone() *PartitionGroupRefInfo {
	nr := *r
	return &nr
}
