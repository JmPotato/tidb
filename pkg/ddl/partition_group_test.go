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

package ddl_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/pingcap/tidb/pkg/ddl"
	"github.com/pingcap/tidb/pkg/domain/affinity"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/auth"
	"github.com/pingcap/tidb/pkg/testkit"
	"github.com/pingcap/tidb/pkg/testkit/external"
	"github.com/stretchr/testify/require"
	pdhttp "github.com/tikv/pd/client/http"
)

func enablePartitionGroup(tk *testkit.TestKit) {
	tk.MustExec("set global tidb_enable_partition_group = on")
}

func getPartitionGroup(t *testing.T, store kv.Storage, name string) *model.PartitionGroupInfo {
	groups, err := ddl.ListPartitionGroups(store)
	require.NoError(t, err)
	for _, g := range groups {
		if g.Name.L == name {
			return g
		}
	}
	return nil
}

func partitionIDs(t *testing.T, tk *testkit.TestKit, table string) []int64 {
	tbl := memberTable(t, tk, table)
	defs := tbl.GetPartitionInfo().Definitions
	ids := make([]int64, len(defs))
	for i, def := range defs {
		ids[i] = def.ID
	}
	return ids
}

func memberTable(t *testing.T, tk *testkit.TestKit, table string) *model.TableInfo {
	return external.GetTableByName(t, tk, "test", table).Meta()
}

// requireSlot checks that partition i of the table uses the given slot of the group.
func requireSlot(t *testing.T, g *model.PartitionGroupInfo, ids []int64, slot uint64) {
	require.Len(t, ids, int(g.PartitionNum))
	for i, id := range ids {
		require.Equal(t, g.PhysicalID(uint64(i), slot), id, "partition %d", i)
	}
}

func TestPartitionGroupLifecycle(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")

	tk.MustGetErrMsg("create partition group pg partitions 4",
		"[ddl:8277]Partition group error: partition group is disabled, set GLOBAL tidb_enable_partition_group = ON to use it")
	enablePartitionGroup(tk)

	tk.MustExec("create partition group pg partitions 4 max_members 8")
	tk.MustGetErrMsg("create partition group pg partitions 4", "[ddl:8277]Partition group 'pg' already exists")
	tk.MustExec("create partition group if not exists pg partitions 4")
	g := getPartitionGroup(t, store, "pg")
	require.NotNil(t, g)
	require.Equal(t, model.StatePublic, g.State)
	require.EqualValues(t, 4, g.PartitionNum)
	require.EqualValues(t, 8, g.SlotSize)
	require.Len(t, g.AffinityGroupIDs, 4)
	for i, id := range g.AffinityGroupIDs {
		// NextGen keyspaces add the keyspace ID because PD group IDs are cluster-wide.
		require.Regexp(t, fmt.Sprintf(`^_tidb_(ks\d+_)?pg%d_%d$`, g.ID, i), id)
	}
	pdGroups, err := affinity.GetGroups(context.Background(), g.AffinityGroupIDs)
	require.NoError(t, err)
	require.Len(t, pdGroups, 4)

	// Members may use different column names; partition i of every member
	// comes from range i of the group, one slot per member.
	tk.MustExec("create table w (w_id int primary key, v int) partition_group = 'pg' partition by hash(w_id) partitions 4")
	tk.MustExec("create table s (s_i_id int, s_w_id bigint, primary key (s_w_id, s_i_id)) partition_group = 'PG' partition by hash(s_w_id) partitions 4")
	requireSlot(t, g, partitionIDs(t, tk, "w"), 0)
	requireSlot(t, g, partitionIDs(t, tk, "s"), 1)
	require.Equal(t, &model.PartitionGroupRefInfo{ID: g.ID, Name: ast.NewCIStr("pg")}, memberTable(t, tk, "s").PartitionGroup)

	tk.MustQuery("show create table w").CheckContain("/*T![partition_group] PARTITION_GROUP='pg' */")
	tk.MustQuery("show partition groups").Check(testkit.Rows(
		fmt.Sprintf("pg %d public 4 8 6 2 test.s,test.w", g.ID)))
	tk.MustQuery("show partition groups like 'x%'").Check(testkit.Rows())
	tk.MustQuery("show partition group status pg").CheckContain(fmt.Sprintf("pg%d_3", g.ID))

	// Data stays readable and writable, and lands in the expected partition.
	tk.MustExec("insert into w values (1, 10), (2, 20), (5, 50)")
	tk.MustQuery("select v from w partition (p1) order by v").Check(testkit.Rows("10", "50"))

	// TRUNCATE takes a new slot; TRUNCATE PARTITION replaces only that partition.
	tk.MustExec("truncate table w")
	requireSlot(t, getPartitionGroup(t, store, "pg"), partitionIDs(t, tk, "w"), 2)
	tk.MustExec("alter table s truncate partition p2")
	ids := partitionIDs(t, tk, "s")
	require.Equal(t, g.PhysicalID(2, 3), ids[2])
	require.Equal(t, g.PhysicalID(1, 1), ids[1])
	require.EqualValues(t, 4, getPartitionGroup(t, store, "pg").NextSlot)

	// A group with members, current or recoverable, cannot be dropped.
	tk.MustContainErrMsg("drop partition group pg", "is still referenced by test.s, test.w")
	timeBeforeDrop, _, safePointSQL, resetGC := MockGC(tk)
	defer resetGC()
	tk.MustExec(fmt.Sprintf(safePointSQL, timeBeforeDrop))
	tk.MustExec("drop table w, s")
	tk.MustContainErrMsg("drop partition group pg", "recoverable")
	tk.MustExec("recover table w")
	requireSlot(t, g, partitionIDs(t, tk, "w"), 2)

	// A group without references can be dropped, and its PD groups go away.
	tk.MustExec("create partition group pg2 partitions 2")
	g2 := getPartitionGroup(t, store, "pg2")
	tk.MustExec("drop partition group pg2")
	require.Nil(t, getPartitionGroup(t, store, "pg2"))
	pdGroups, err = affinity.GetGroups(context.Background(), g2.AffinityGroupIDs)
	require.NoError(t, err)
	require.Empty(t, pdGroups)
	tk.MustExec("drop partition group if exists pg2")
	tk.MustGetErrMsg("drop partition group pg2", "[ddl:8277]Unknown partition group 'pg2'")
}

func TestPartitionGroupMemberValidation(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	enablePartitionGroup(tk)
	tk.MustExec("create partition group pg partitions 4")
	tk.MustExec("create placement policy p1 followers=1")

	for _, c := range []struct{ sql, err string }{
		{"create table t (a int) partition_group = 'nope' partition by hash(a) partitions 4", "Unknown partition group 'nope'"},
		{"create table t (a int) partition_group = 'pg'", "member tables must be partitioned by HASH"},
		{"create table t (a int) partition_group = 'pg' partition by key(a) partitions 4", "member tables must be partitioned by HASH"},
		{"create table t (a int) partition_group = 'pg' partition by range(a) (partition p0 values less than (10))", "member tables must be partitioned by HASH"},
		{"create table t (a int) partition_group = 'pg' partition by hash(a) partitions 8", "member tables must have 4 partitions"},
		{"create table t (a int) partition_group = 'pg' partition by hash(a + 1) partitions 4", "single integer column"},
		{"create table t (a int, b int) partition_group = 'pg' affinity = 'partition' partition by hash(a) partitions 4", "AFFINITY option is not supported"},
		{"create table t (a int) partition_group = 'pg' placement policy = p1 partition by hash(a) partitions 4", "placement policies are not supported"},
		{"create table t (a int) partition_group = 'pg' partition by hash(a) (partition p0 placement policy = p1, partition p1, partition p2, partition p3)", "placement policies are not supported"},
		{"create table t (a int, b int, unique key (b) global) partition_group = 'pg' partition by hash(a) partitions 4", "global indexes are not supported"},
	} {
		tk.MustContainErrMsg(c.sql, c.err)
	}
	require.EqualValues(t, 0, getPartitionGroup(t, store, "pg").NextSlot, "rejected statements must not take slots")

	tk.MustExec("set global tidb_enable_partition_group = off")
	tk.MustContainErrMsg("create table t (a int) partition_group = 'pg' partition by hash(a) partitions 4", "partition group is disabled")
}

func TestPartitionGroupRejectedDDL(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	enablePartitionGroup(tk)
	tk.MustExec("create partition group pg partitions 4")
	tk.MustExec("create placement policy p1 followers=1")
	tk.MustExec("create table t (a int, b int) partition_group = 'pg' partition by hash(a) partitions 4")
	tk.MustExec("create table nt (a int, b int)")

	for _, sql := range []string{
		"alter table t add partition partitions 1",
		"alter table t coalesce partition 1",
		"alter table t reorganize partition p0, p1 into (partition p0)",
		"alter table t exchange partition p0 with table nt",
		"alter table t partition by hash(b) partitions 4",
		"alter table t remove partitioning",
		"alter table t affinity = 'partition'",
		"alter table t placement policy = p1",
		"alter table t partition p0 placement policy = p1",
		"alter table nt partition_group = 'pg'",
		"create table t2 like t",
		"split table t between (0) and (100) regions 4",
	} {
		err := tk.ExecToErr(sql)
		require.ErrorContains(t, err, "Unsupported", sql)
	}
	// Local indexes and RENAME keep the contract.
	tk.MustExec("alter table t add index idx_b (b)")
	tk.MustExec("rename table t to t3")
	require.Equal(t, "pg", memberTable(t, tk, "t3").PartitionGroup.Name.L)
}

func TestPartitionGroupSlotExhaustion(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	enablePartitionGroup(tk)
	tk.MustExec("create partition group pg partitions 2 max_members 2")
	tk.MustExec("create table t1 (a int) partition_group = 'pg' partition by hash(a) partitions 2")
	tk.MustQuery("show warnings").Check(testkit.Rows())
	tk.MustExec("create table t2 (a int) partition_group = 'pg' partition by hash(a) partitions 2")
	tk.MustQuery("show warnings").CheckContain("has 0 of 2 member slots left")
	tk.MustContainErrMsg("create table t3 (a int) partition_group = 'pg' partition by hash(a) partitions 2", "has no member slots left")
	tk.MustContainErrMsg("truncate table t1", "has no member slots left")
	tk.MustContainErrMsg("alter table t1 truncate partition p0", "has no member slots left")
	// Existing members keep working after the group runs out of slots.
	tk.MustExec("insert into t1 values (1), (2)")
	tk.MustQuery("select count(*) from t1").Check(testkit.Rows("2"))

	tk.MustGetErrMsg("create partition group bad partitions 0", "[ddl:8277]Partition group error: PARTITIONS must be between 1 and 8192")
	tk.MustContainErrMsg("create partition group bad partitions 4 max_members 100000", "MAX_MEMBERS must be between 1 and 65536")
}

func TestPartitionGroupConcurrentMembers(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	enablePartitionGroup(tk)
	tk.MustExec("create partition group pg partitions 4")

	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tki := testkit.NewTestKit(t, store)
			tki.MustExec(fmt.Sprintf("create table test.t%d (a int) partition_group = 'pg' partition by hash(a) partitions 4", i))
		}()
	}
	wg.Wait()

	g := getPartitionGroup(t, store, "pg")
	require.EqualValues(t, n, g.NextSlot)
	seen := make(map[uint64]string)
	for i := range n {
		table := fmt.Sprintf("t%d", i)
		ids := partitionIDs(t, tk, table)
		slot := uint64(ids[0] - g.RangeStart(0))
		requireSlot(t, g, ids, slot)
		require.NotContains(t, seen, slot, "%s reuses the slot of %s", table, seen[slot])
		seen[slot] = table
	}
}

func TestShowPartitionGroupsHidesInvisibleTables(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	enablePartitionGroup(tk)
	tk.MustExec("create partition group pg partitions 2")
	tk.MustExec("create table t (a int) partition_group = 'pg' partition by hash(a) partitions 2")
	tk.MustExec("create user u1")

	tk1 := testkit.NewTestKit(t, store)
	require.NoError(t, tk1.Session().Auth(&auth.UserIdentity{Username: "u1", Hostname: "%"}, nil, nil, nil))
	tk1.MustQuery("show partition groups").CheckAt([]int{0, 6, 7}, testkit.Rows("pg 0 "))
	tk.MustExec("grant select on test.t to u1")
	tk1.MustQuery("show partition groups").CheckAt([]int{0, 6, 7}, testkit.Rows("pg 1 test.t"))
}

// Signedness is deliberately not validated: HASH partitioning maps signed and
// unsigned integers through the same bit pattern, so a value that both members
// can store lands in the same partition index of both.
func TestPartitionGroupSignedAndUnsignedMembersAgree(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	enablePartitionGroup(tk)
	tk.MustExec("create partition group pg partitions 4")
	tk.MustExec("create table s (a int) partition_group = 'pg' partition by hash(a) partitions 4")
	tk.MustExec("create table u (a bigint unsigned) partition_group = 'pg' partition by hash(a) partitions 4")
	values := "(0), (1), (2), (3), (5), (1023), (2147483647)"
	tk.MustExec("insert into s values " + values)
	tk.MustExec("insert into u values " + values)
	for p := range 4 {
		want := tk.MustQuery(fmt.Sprintf("select a from s partition (p%d) order by a", p)).Rows()
		tk.MustQuery(fmt.Sprintf("select a from u partition (p%d) order by a", p)).Check(want)
	}
}

// storesUnavailablePDClient fails to list stores, so a create job keeps failing
// after it has created the PD affinity groups.
type storesUnavailablePDClient struct{ pdhttp.Client }

func (storesUnavailablePDClient) GetStores(context.Context) (*pdhttp.StoresInfo, error) {
	return nil, errors.New("mock: store 1 is busy")
}

func (storesUnavailablePDClient) GetScheduleConfig(context.Context) (map[string]any, error) {
	return map[string]any{"affinity-schedule-limit": float64(1)}, nil
}

// A create job cancelled after too many retries keeps its args when it starts
// rolling back, so the rollback removes the PD groups instead of panicking.
func TestPartitionGroupCreateRollsBackAfterRetries(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	enablePartitionGroup(tk)
	tk.MustExec("set global tidb_ddl_error_count_limit = 3")
	defer tk.MustExec("set global tidb_ddl_error_count_limit = default")
	defer affinity.SetPDClientForTest(storesUnavailablePDClient{})()

	tk.MustContainErrMsg("create partition group pg partitions 4", "mock: store 1 is busy")
	require.Nil(t, getPartitionGroup(t, store, "pg"))
	job := tk.MustQuery("admin show ddl jobs 1").Rows()[0]
	require.Equal(t, "create partition group", job[3])
	require.Equal(t, "rollback done", job[len(job)-2])

	ids := make([]string, 4)
	for i := range ids {
		ids[i] = fmt.Sprintf("_tidb_pg%s_%d", job[5], i)
		if codec := store.GetCodec(); len(codec.GetKeyspace()) > 0 {
			ids[i] = fmt.Sprintf("_tidb_ks%d_pg%s_%d", codec.GetKeyspaceID(), job[5], i)
		}
	}
	pdGroups, err := affinity.GetGroups(context.Background(), ids)
	require.NoError(t, err)
	require.Empty(t, pdGroups)
}
