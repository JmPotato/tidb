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

package ddl

import (
	"context"
	"fmt"
	"strings"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/ddl/logutil"
	"github.com/pingcap/tidb/pkg/domain/affinity"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/meta"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/sessionctx/vardef"
	"github.com/pingcap/tidb/pkg/tablecodec"
	"github.com/pingcap/tidb/pkg/util/dbterror"
	tikv "github.com/tikv/client-go/v2/tikv"
	pdhttp "github.com/tikv/pd/client/http"
	"go.uber.org/zap"
)

// pdAffinityGroupBatchSize keeps every create or delete request within one PD
// etcd transaction: PD writes two etcd operations per group and splits a
// request into transactions of 120 operations.
const pdAffinityGroupBatchSize = 60

// partitionGroupAffinityGroupID returns the PD affinity group ID of partition
// index idx. The name embeds the globally unique partition group ID, so a group
// with this name can only have been created by this partition group. In a
// keyspace the keyspace ID is included because PD affinity group IDs are not
// scoped by keyspace.
func partitionGroupAffinityGroupID(codec tikv.Codec, groupID int64, idx uint64) string {
	if codec != nil && len(codec.GetKeyspace()) > 0 {
		return fmt.Sprintf("_tidb_ks%d_pg%d_%d", codec.GetKeyspaceID(), groupID, idx)
	}
	return fmt.Sprintf("_tidb_pg%d_%d", groupID, idx)
}

func partitionGroupKeyRange(codec tikv.Codec, g *model.PartitionGroupInfo, idx uint64) pdhttp.AffinityGroupKeyRange {
	startKey := tablecodec.EncodeTablePrefix(g.RangeStart(idx))
	endKey := tablecodec.EncodeTablePrefix(g.RangeEnd(idx))
	if codec != nil {
		startKey, endKey = codec.EncodeRegionRange(startKey, endKey)
	}
	return pdhttp.AffinityGroupKeyRange{StartKey: startKey, EndKey: endKey}
}

// getPartitionGroupByName reads a partition group from the latest meta.
// Partition groups are few and only read by DDL and SHOW, so they are not
// cached in the infoschema.
func getPartitionGroupByName(store kv.Storage, name ast.CIStr) (*model.PartitionGroupInfo, error) {
	groups, err := listPartitionGroups(store)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g.Name.L == name.L {
			return g, nil
		}
	}
	return nil, nil
}

func listPartitionGroups(store kv.Storage) ([]*model.PartitionGroupInfo, error) {
	ver, err := store.CurrentVersion(kv.GlobalTxnScope)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return meta.NewReader(store.GetSnapshot(ver)).ListPartitionGroups()
}

// ListPartitionGroups returns all partition groups. It is used by SHOW.
func ListPartitionGroups(store kv.Storage) ([]*model.PartitionGroupInfo, error) {
	return listPartitionGroups(store)
}

// PartitionGroupAffinityKeyRange returns the PD key range of partition index idx.
func PartitionGroupAffinityKeyRange(codec tikv.Codec, g *model.PartitionGroupInfo, idx uint64) pdhttp.AffinityGroupKeyRange {
	return partitionGroupKeyRange(codec, g, idx)
}

func checkPartitionGroupEnabled() error {
	if !vardef.EnablePartitionGroup.Load() {
		return dbterror.ErrInvalidPartitionGroup.GenWithStackByArgs("partition group is disabled, set GLOBAL tidb_enable_partition_group = ON to use it")
	}
	return nil
}

// CreatePartitionGroup implements the DDL executor interface.
func (e *executor) CreatePartitionGroup(ctx sessionctx.Context, stmt *ast.CreatePartitionGroupStmt) error {
	if err := checkPartitionGroupEnabled(); err != nil {
		return err
	}
	if stmt.Partitions == 0 || stmt.Partitions > mysql.PartitionCountLimit {
		return dbterror.ErrInvalidPartitionGroup.GenWithStackByArgs(
			fmt.Sprintf("PARTITIONS must be between 1 and %d", mysql.PartitionCountLimit))
	}
	slotSize := stmt.MaxMembers
	if slotSize == 0 {
		slotSize = model.DefaultPartitionGroupMaxMembers
	}
	if slotSize > model.MaxPartitionGroupMaxMembers {
		return dbterror.ErrInvalidPartitionGroup.GenWithStackByArgs(
			fmt.Sprintf("MAX_MEMBERS must be between 1 and %d", model.MaxPartitionGroupMaxMembers))
	}

	existing, err := getPartitionGroupByName(e.store, stmt.Name)
	if err != nil {
		return err
	}
	if existing != nil {
		err = dbterror.ErrPartitionGroupExists.GenWithStackByArgs(stmt.Name.O)
		if stmt.IfNotExists {
			ctx.GetSessionVars().StmtCtx.AppendNote(err)
			return nil
		}
		return err
	}

	info := &model.PartitionGroupInfo{
		Name:         stmt.Name,
		PartitionNum: stmt.Partitions,
		SlotSize:     slotSize,
	}
	job := &model.Job{
		Version:        model.GetJobVerInUse(),
		SchemaName:     stmt.Name.L,
		Type:           model.ActionCreatePartitionGroup,
		BinlogInfo:     &model.HistoryInfo{},
		CDCWriteSource: ctx.GetSessionVars().CDCWriteSource,
		InvolvingSchemaInfo: []model.InvolvingSchemaInfo{{
			PartitionGroup: stmt.Name.L,
		}},
		SQLMode: ctx.GetSessionVars().SQLMode,
	}
	if err = e.doDDLJob2(ctx, job, &model.PartitionGroupArgs{PGInfo: info}); err != nil {
		if stmt.IfNotExists && dbterror.ErrPartitionGroupExists.Equal(err) {
			ctx.GetSessionVars().StmtCtx.AppendNote(err)
			return nil
		}
		return err
	}
	if limit, ok := affinity.GetAffinityScheduleLimit(e.ctx); ok && limit == 0 {
		ctx.GetSessionVars().StmtCtx.AppendWarning(dbterror.ErrInvalidPartitionGroup.FastGenByArgs(
			"PD schedule.affinity-schedule-limit is 0, so PD will not merge or place the ranges of this partition group"))
	}
	return nil
}

// DropPartitionGroup implements the DDL executor interface.
func (e *executor) DropPartitionGroup(ctx sessionctx.Context, stmt *ast.DropPartitionGroupStmt) error {
	g, err := getPartitionGroupByName(e.store, stmt.Name)
	if err != nil {
		return err
	}
	if g == nil {
		err = dbterror.ErrPartitionGroupNotExists.GenWithStackByArgs(stmt.Name.O)
		if stmt.IfExists {
			ctx.GetSessionVars().StmtCtx.AppendNote(err)
			return nil
		}
		return err
	}
	job := &model.Job{
		Version:        model.GetJobVerInUse(),
		SchemaID:       g.ID,
		SchemaName:     g.Name.L,
		Type:           model.ActionDropPartitionGroup,
		BinlogInfo:     &model.HistoryInfo{},
		CDCWriteSource: ctx.GetSessionVars().CDCWriteSource,
		InvolvingSchemaInfo: []model.InvolvingSchemaInfo{{
			PartitionGroup: g.Name.L,
		}},
		SQLMode: ctx.GetSessionVars().SQLMode,
	}
	return e.doDDLJob2(ctx, job, &model.PartitionGroupArgs{PGInfo: g})
}

func onCreatePartitionGroup(jobCtx *jobContext, job *model.Job) (ver int64, _ error) {
	args, err := model.GetPartitionGroupArgs(job)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}
	g := args.PGInfo
	codec := jobCtx.store.GetCodec()
	g.AffinityGroupIDs = make([]string, g.PartitionNum)
	for i := range g.PartitionNum {
		g.AffinityGroupIDs[i] = partitionGroupAffinityGroupID(codec, g.ID, i)
	}

	if job.IsRollingback() {
		// The affinity group names embed the unique group ID, so every group
		// with these names was created by this job.
		if err := deleteAffinityGroupsInBatches(jobCtx.stepCtx, g.AffinityGroupIDs); err != nil {
			return ver, errors.Trace(err)
		}
		job.FinishDBJob(model.JobStateRollbackDone, model.StateNone, ver, nil)
		return ver, nil
	}

	groups, err := jobCtx.metaMut.ListPartitionGroups()
	if err != nil {
		return ver, errors.Trace(err)
	}
	for _, other := range groups {
		if other.Name.L == g.Name.L && other.ID != g.ID {
			job.State = model.JobStateCancelled
			return ver, dbterror.ErrPartitionGroupExists.GenWithStackByArgs(g.Name.O)
		}
	}

	// Creating the PD groups is idempotent, so a retry after an owner change
	// or a PD error continues from where the previous attempt stopped.
	if err := createPartitionGroupAffinityGroups(jobCtx.stepCtx, codec, g); err != nil {
		logutil.DDLLogger().Warn("create partition group affinity groups failed",
			zap.String("group", g.Name.O), zap.Error(err))
		return ver, errors.Trace(err)
	}

	g.State = model.StatePublic
	if err := jobCtx.metaMut.CreatePartitionGroup(g); err != nil {
		return ver, errors.Trace(err)
	}
	job.SchemaID = g.ID
	ver, err = updateSchemaVersion(jobCtx, job)
	if err != nil {
		return ver, errors.Trace(err)
	}
	job.FinishDBJob(model.JobStateDone, model.StatePublic, ver, nil)
	return ver, nil
}

// createPartitionGroupAffinityGroups registers one PD affinity group per
// partition index and spreads their leaders over the TiKV stores. Without an
// explicit leader, PD adopts the leader of the first Region it sees, which
// tends to put every group on the same store.
func createPartitionGroupAffinityGroups(ctx context.Context, codec tikv.Codec, g *model.PartitionGroupInfo) error {
	for start := uint64(0); start < g.PartitionNum; start += pdAffinityGroupBatchSize {
		end := min(start+pdAffinityGroupBatchSize, g.PartitionNum)
		batch := make(map[string][]pdhttp.AffinityGroupKeyRange, end-start)
		for i := start; i < end; i++ {
			batch[g.AffinityGroupIDs[i]] = []pdhttp.AffinityGroupKeyRange{partitionGroupKeyRange(codec, g, i)}
		}
		if err := affinity.CreateGroupsIfNotExists(ctx, batch); err != nil {
			return errors.Trace(err)
		}
	}

	stores, err := affinity.ListUpStoreIDs(ctx)
	if err != nil {
		return errors.Trace(err)
	}
	if len(stores) == 0 {
		return nil
	}
	for i, id := range g.AffinityGroupIDs {
		leader := stores[i%len(stores)]
		if err := affinity.UpdateGroupPeers(ctx, id, leader, stores); err != nil {
			return errors.Annotatef(err, "set leader of affinity group %s", id)
		}
	}
	return nil
}

func deleteAffinityGroupsInBatches(ctx context.Context, ids []string) error {
	for start := 0; start < len(ids); start += pdAffinityGroupBatchSize {
		end := min(start+pdAffinityGroupBatchSize, len(ids))
		if err := affinity.DeleteGroups(ctx, ids[start:end]); err != nil && !isAffinityGroupNotFound(err) {
			return errors.Trace(err)
		}
	}
	return nil
}

// isAffinityGroupNotFound reports whether PD rejected a request because the
// group does not exist (HTTP 404, ErrAffinityGroupNotFound).
func isAffinityGroupNotFound(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "ErrAffinityGroupNotFound") || strings.Contains(msg, "status: '404")
}

func onDropPartitionGroup(jobCtx *jobContext, job *model.Job) (ver int64, _ error) {
	args, err := model.GetPartitionGroupArgs(job)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}
	g, err := jobCtx.metaMut.GetPartitionGroup(args.PGInfo.ID)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, dbterror.ErrPartitionGroupNotExists.GenWithStackByArgs(args.PGInfo.Name.O)
	}

	switch g.State {
	case model.StatePublic:
		members, err := partitionGroupReferences(jobCtx, g.ID)
		if err != nil {
			return ver, errors.Trace(err)
		}
		if len(members) > 0 {
			job.State = model.JobStateCancelled
			return ver, dbterror.ErrInvalidPartitionGroup.GenWithStackByArgs(fmt.Sprintf(
				"partition group %s is still referenced by %s (including dropped tables that can be recovered)",
				g.Name.O, strings.Join(members, ", ")))
		}
		// Mark the group as being deleted first, so that the PD cleanup can be
		// retried and new members can no longer take slots.
		g.State = model.StateDeleteOnly
		if err := jobCtx.metaMut.UpdatePartitionGroup(g); err != nil {
			return ver, errors.Trace(err)
		}
		job.SchemaState = model.StateDeleteOnly
		return ver, nil
	case model.StateDeleteOnly:
		if err := deleteAffinityGroupsInBatches(jobCtx.stepCtx, g.AffinityGroupIDs); err != nil {
			return ver, errors.Trace(err)
		}
		if err := jobCtx.metaMut.DropPartitionGroup(g.ID); err != nil {
			return ver, errors.Trace(err)
		}
		ver, err = updateSchemaVersion(jobCtx, job)
		if err != nil {
			return ver, errors.Trace(err)
		}
		job.FinishDBJob(model.JobStateDone, model.StateNone, ver, nil)
		return ver, nil
	default:
		return ver, dbterror.ErrInvalidDDLState.GenWithStackByArgs("partition group", g.State)
	}
}

// checkPartitionGroupMember resolves the partition group of a new member table
// and checks the partitioning contract: HASH partitions on one integer column,
// with exactly as many partitions as the group.
func checkPartitionGroupMember(store kv.Storage, tbInfo *model.TableInfo) error {
	ref := tbInfo.PartitionGroup
	if ref == nil {
		return nil
	}
	if err := checkPartitionGroupEnabled(); err != nil {
		return err
	}
	g, err := getPartitionGroupByName(store, ref.Name)
	if err != nil {
		return err
	}
	if g == nil || g.State != model.StatePublic {
		return dbterror.ErrPartitionGroupNotExists.GenWithStackByArgs(ref.Name.O)
	}
	ref.ID, ref.Name = g.ID, g.Name

	invalid := func(reason string) error {
		return dbterror.ErrInvalidPartitionGroup.GenWithStackByArgs(
			fmt.Sprintf("table %s cannot join partition group %s: %s", tbInfo.Name.O, g.Name.O, reason))
	}
	pi := tbInfo.GetPartitionInfo()
	if pi == nil || pi.Type != ast.PartitionTypeHash {
		return invalid("member tables must be partitioned by HASH")
	}
	if uint64(len(pi.Definitions)) != g.PartitionNum {
		return invalid(fmt.Sprintf("member tables must have %d partitions", g.PartitionNum))
	}
	col := tbInfo.FindPublicColumnByName(strings.Trim(strings.TrimSpace(pi.Expr), "`"))
	if col == nil || !mysql.IsIntegerType(col.GetType()) {
		return invalid("the partition expression must be a single integer column")
	}
	if tbInfo.TempTableType != model.TempTableNone {
		return invalid("temporary tables are not supported")
	}
	if tbInfo.Affinity != nil {
		return invalid("the AFFINITY option is not supported")
	}
	if tbInfo.PlacementPolicyRef != nil {
		return invalid("placement policies are not supported")
	}
	for _, def := range pi.Definitions {
		if def.PlacementPolicyRef != nil {
			return invalid("placement policies are not supported")
		}
	}
	for _, idx := range tbInfo.Indices {
		if idx.Global {
			return invalid("global indexes are not supported")
		}
	}
	return nil
}

// checkPartitionGroupMemberOnOwner makes sure that the group of a new member
// still exists when the job runs, and that the partition IDs come from the
// group's reserved ranges. The job holds the group shared in the DDL job
// scheduler, so DROP PARTITION GROUP cannot run at the same time.
func checkPartitionGroupMemberOnOwner(m *meta.Mutator, tbInfo *model.TableInfo) error {
	ref := tbInfo.PartitionGroup
	if ref == nil {
		return nil
	}
	g, err := m.GetPartitionGroup(ref.ID)
	if err != nil || g.State != model.StatePublic {
		return dbterror.ErrPartitionGroupNotExists.GenWithStackByArgs(ref.Name.O)
	}
	for i, def := range tbInfo.GetPartitionInfo().Definitions {
		if def.ID < g.RangeStart(uint64(i)) || def.ID >= g.RangeEnd(uint64(i)) {
			return errors.Errorf("partition %s of table %s has ID %d outside of partition group %s range %d",
				def.Name.O, tbInfo.Name.O, def.ID, g.Name.O, i)
		}
	}
	return nil
}

// rejectPartitionGroupMember returns an error for DDL that would break the
// partition group contract of a member table.
func rejectPartitionGroupMember(tbInfo *model.TableInfo, op string) error {
	if tbInfo != nil && tbInfo.PartitionGroup != nil {
		return dbterror.ErrGeneralUnsupportedDDL.GenWithStackByArgs(op + " of a partition group member table")
	}
	return nil
}

// maxHistoryJobsForPartitionGroupReferences bounds the history scan that looks
// for dropped or truncated members that can still be recovered.
const maxHistoryJobsForPartitionGroupReferences = 4096

// partitionGroupReferences returns the tables that reference the group: the
// current tables, and the dropped or truncated tables whose metadata is still
// readable before the GC safe point, because RECOVER/FLASHBACK TABLE would
// bring them back with physical IDs from the group's ranges.
func partitionGroupReferences(jobCtx *jobContext, groupID int64) ([]string, error) {
	var refs []string
	m := jobCtx.metaMut
	dbs, err := m.ListDatabases()
	if err != nil {
		return nil, errors.Trace(err)
	}
	for _, db := range dbs {
		err := m.IterTables(db.ID, func(tbl *model.TableInfo) error {
			if tbl.PartitionGroup != nil && tbl.PartitionGroup.ID == groupID {
				refs = append(refs, db.Name.O+"."+tbl.Name.O)
			}
			return nil
		})
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	store := jobCtx.store
	ver, err := store.CurrentVersion(kv.GlobalTxnScope)
	if err != nil {
		return nil, errors.Trace(err)
	}
	iter, err := meta.NewReader(store.GetSnapshot(ver)).GetLastHistoryDDLJobsIterator()
	if err != nil {
		return nil, errors.Trace(err)
	}
	jobs, err := iter.GetLastJobs(maxHistoryJobsForPartitionGroupReferences, nil)
	if err != nil {
		return nil, errors.Trace(err)
	}
	for _, job := range jobs {
		if job.Type != model.ActionDropTable && job.Type != model.ActionTruncateTable {
			continue
		}
		snap := store.GetSnapshot(kv.NewVersion(GetRecoverSnapshotTS(job)))
		tbl, err := meta.NewReader(snap).GetTable(job.SchemaID, job.TableID)
		if err != nil || tbl == nil {
			// The snapshot is past the GC safe point or the job did not drop a
			// table, so the table cannot be recovered.
			continue
		}
		if tbl.PartitionGroup != nil && tbl.PartitionGroup.ID == groupID {
			refs = append(refs, fmt.Sprintf("%s.%s (%s, recoverable)", job.SchemaName, tbl.Name.O, job.Type))
		}
	}
	return refs, nil
}

// warnPartitionGroupLowSlots warns when fewer than 10% of the member slots of
// the table's partition group are left, since slots are never reused.
func warnPartitionGroupLowSlots(ctx sessionctx.Context, store kv.Storage, tbInfo *model.TableInfo) {
	if tbInfo.PartitionGroup == nil {
		return
	}
	g, err := getPartitionGroupByName(store, tbInfo.PartitionGroup.Name)
	if err != nil || g == nil {
		return
	}
	if remaining := g.RemainingSlots(); remaining*10 < g.SlotSize {
		ctx.GetSessionVars().StmtCtx.AppendWarning(dbterror.ErrInvalidPartitionGroup.FastGenByArgs(fmt.Sprintf(
			"partition group %s has %d of %d member slots left; every member table, TRUNCATE TABLE and TRUNCATE PARTITION takes one",
			g.Name.O, remaining, g.SlotSize)))
	}
}
