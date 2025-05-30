// Copyright 2024 PingCAP, Inc.
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

package snapclient

import (
	"context"
	"time"

	"github.com/pingcap/errors"
	backuppb "github.com/pingcap/kvproto/pkg/brpb"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/br/pkg/glue"
	"github.com/pingcap/tidb/br/pkg/metautil"
	restoreutils "github.com/pingcap/tidb/br/pkg/restore/utils"
	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/br/pkg/summary"
	"github.com/pingcap/tidb/br/pkg/utils"
	"github.com/pingcap/tidb/pkg/domain/infosync"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/meta/model"
	statstypes "github.com/pingcap/tidb/pkg/statistics/handle/types"
	tidbutil "github.com/pingcap/tidb/pkg/util"
	"github.com/pingcap/tidb/pkg/util/engine"
	pdhttp "github.com/tikv/pd/client/http"
	"go.uber.org/multierr"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const defaultChannelSize = 1024

// defaultChecksumConcurrency is the default number of the concurrent
// checksum tasks.
const defaultChecksumConcurrency = 64

// CreatedTable is a table created on restore process,
// but not yet filled with data.
type CreatedTable struct {
	RewriteRule *restoreutils.RewriteRules
	Table       *model.TableInfo
	OldTable    *metautil.Table
}

type PhysicalTable struct {
	NewPhysicalID int64
	OldPhysicalID int64
	RewriteRules  *restoreutils.RewriteRules
	Files         []*backuppb.File
}

func defaultOutputTableChan() chan *CreatedTable {
	return make(chan *CreatedTable, defaultChannelSize)
}

// ExhaustErrors drains all remaining errors in the channel, into a slice of errors.
func ExhaustErrors(ec <-chan error) []error {
	out := make([]error, 0, len(ec))
	for {
		select {
		case err := <-ec:
			out = append(out, err)
		default:
			// errCh will NEVER be closed(ya see, it has multi sender-part),
			// so we just consume the current backlog of this channel, then return.
			return out
		}
	}
}

type PipelineContext struct {
	// pipeline item switch
	Checksum         bool
	LoadStats        bool
	WaitTiflashReady bool

	// pipeline item configuration
	LogProgress         bool
	ChecksumConcurrency uint
	StatsConcurrency    uint
	AutoAnalyze         bool

	// pipeline item tool client
	KvClient   kv.Client
	ExtStorage storage.ExternalStorage
	Glue       glue.Glue
}

// RestorePipeline does checksum, load stats and wait for tiflash to be ready.
func (rc *SnapClient) RestorePipeline(ctx context.Context, plCtx PipelineContext, createdTables []*CreatedTable) (err error) {
	start := time.Now()
	defer func() {
		summary.CollectDuration("restore pipeline", time.Since(start))
	}()
	// We make bigger errCh so we won't block on multi-part failed.
	errCh := make(chan error, 32)
	postHandleCh := afterTableRestoredCh(ctx, createdTables)
	progressLen := int64(0)
	if plCtx.Checksum {
		progressLen += int64(len(createdTables))
	}
	progressLen += int64(len(createdTables)) // for pipeline item - update stats meta
	if plCtx.WaitTiflashReady {
		progressLen += int64(len(createdTables))
	}

	// Redirect to log if there is no log file to avoid unreadable output.
	updateCh := plCtx.Glue.StartProgress(ctx, "Restore Pipeline", progressLen, !plCtx.LogProgress)
	defer updateCh.Close()
	// pipeline checksum
	if plCtx.Checksum {
		postHandleCh = rc.GoValidateChecksum(ctx, postHandleCh, plCtx.KvClient, errCh, updateCh, plCtx.ChecksumConcurrency)
	}

	// pipeline update meta and load stats
	postHandleCh = rc.GoUpdateMetaAndLoadStats(ctx, plCtx.ExtStorage, postHandleCh, errCh, updateCh, plCtx.StatsConcurrency, plCtx.AutoAnalyze, plCtx.LoadStats)

	// pipeline wait Tiflash synced
	if plCtx.WaitTiflashReady {
		postHandleCh = rc.GoWaitTiFlashReady(ctx, postHandleCh, updateCh, errCh)
	}

	finish := dropToBlackhole(ctx, postHandleCh, errCh)

	select {
	case err = <-errCh:
		err = multierr.Append(err, multierr.Combine(ExhaustErrors(errCh)...))
	case <-finish:
	}

	return errors.Trace(err)
}

func afterTableRestoredCh(ctx context.Context, createdTables []*CreatedTable) <-chan *CreatedTable {
	outCh := make(chan *CreatedTable)

	go func() {
		defer close(outCh)

		for _, createdTable := range createdTables {
			select {
			case <-ctx.Done():
				return
			default:
				outCh <- createdTable
			}
		}
	}()
	return outCh
}

// dropToBlackhole drop all incoming tables into black hole,
// i.e. don't execute checksum, just increase the process anyhow.
func dropToBlackhole(ctx context.Context, inCh <-chan *CreatedTable, errCh chan<- error) <-chan struct{} {
	outCh := make(chan struct{}, 1)
	go func() {
		defer func() {
			close(outCh)
		}()
		for {
			select {
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			case _, ok := <-inCh:
				if !ok {
					return
				}
			}
		}
	}()
	return outCh
}

func concurrentHandleTablesCh(
	ctx context.Context,
	inCh <-chan *CreatedTable,
	outCh chan<- *CreatedTable,
	errCh chan<- error,
	workers *tidbutil.WorkerPool,
	processFun func(context.Context, *CreatedTable) error,
	deferFun func()) {
	eg, ectx := errgroup.WithContext(ctx)
	defer func() {
		if err := eg.Wait(); err != nil {
			errCh <- err
		}
		close(outCh)
		deferFun()
	}()

	for {
		select {
		// if we use ectx here, maybe canceled will mask real error.
		case <-ctx.Done():
			errCh <- ctx.Err()
		case tbl, ok := <-inCh:
			if !ok {
				return
			}
			cloneTable := tbl
			worker := workers.ApplyWorker()
			eg.Go(func() error {
				defer workers.RecycleWorker(worker)
				err := processFun(ectx, cloneTable)
				if err != nil {
					return err
				}
				outCh <- cloneTable
				return nil
			})
		}
	}
}

// GoValidateChecksum forks a goroutine to validate checksum after restore.
// it returns a channel fires a struct{} when all things get done.
func (rc *SnapClient) GoValidateChecksum(
	ctx context.Context,
	inCh <-chan *CreatedTable,
	kvClient kv.Client,
	errCh chan<- error,
	updateCh glue.Progress,
	concurrency uint,
) chan *CreatedTable {
	log.Info("Start to validate checksum")
	outCh := defaultOutputTableChan()
	workers := tidbutil.NewWorkerPool(defaultChecksumConcurrency, "RestoreChecksum")
	go concurrentHandleTablesCh(ctx, inCh, outCh, errCh, workers, func(c context.Context, tbl *CreatedTable) error {
		err := rc.execAndValidateChecksum(c, tbl, kvClient, concurrency)
		if err != nil {
			return errors.Trace(err)
		}
		updateCh.Inc()
		return nil
	}, func() {
		log.Info("all checksum ended")
	})
	return outCh
}

func (rc *SnapClient) GoUpdateMetaAndLoadStats(
	ctx context.Context,
	s storage.ExternalStorage,
	inCh <-chan *CreatedTable,
	errCh chan<- error,
	updateCh glue.Progress,
	statsConcurrency uint,
	autoAnalyze bool,
	loadStats bool,
) chan *CreatedTable {
	log.Info("Start to update meta then load stats")
	outCh := defaultOutputTableChan()
	workers := tidbutil.NewWorkerPool(statsConcurrency, "UpdateStats")
	statsHandler := rc.dom.StatsHandle()

	go concurrentHandleTablesCh(ctx, inCh, outCh, errCh, workers, func(c context.Context, tbl *CreatedTable) error {
		oldTable := tbl.OldTable
		var statsErr error = nil
		if loadStats && oldTable.Stats != nil {
			log.Info("start loads analyze after validate checksum",
				zap.Int64("old id", oldTable.Info.ID),
				zap.Int64("new id", tbl.Table.ID),
			)
			start := time.Now()
			// NOTICE: skip updating cache after load stats from json
			if statsErr = statsHandler.LoadStatsFromJSONNoUpdate(ctx, rc.dom.InfoSchema(), oldTable.Stats, 0); statsErr != nil {
				log.Error("analyze table failed", zap.Any("table", oldTable.Stats), zap.Error(statsErr))
			}
			log.Info("restore stat done",
				zap.Stringer("table", oldTable.Info.Name),
				zap.Stringer("db", oldTable.DB.Name),
				zap.Duration("cost", time.Since(start)))
		} else if loadStats && len(oldTable.StatsFileIndexes) > 0 {
			log.Info("start to load statistic data for each partition",
				zap.Int64("old id", oldTable.Info.ID),
				zap.Int64("new id", tbl.Table.ID),
			)
			start := time.Now()
			rewriteIDMap := restoreutils.GetTableIDMap(tbl.Table, tbl.OldTable.Info)
			if statsErr = metautil.RestoreStats(ctx, s, rc.cipher, statsHandler, tbl.Table, oldTable.StatsFileIndexes, rewriteIDMap); statsErr != nil {
				log.Error("analyze table failed", zap.Any("table", oldTable.StatsFileIndexes), zap.Error(statsErr))
			}
			log.Info("restore statistic data done",
				zap.Stringer("table", oldTable.Info.Name),
				zap.Stringer("db", oldTable.DB.Name),
				zap.Duration("cost", time.Since(start)))
		}

		if statsErr != nil || !loadStats || (oldTable.Stats == nil && len(oldTable.StatsFileIndexes) == 0) {
			// Not need to return err when failed because of update analysis-meta
			log.Info("start update metas", zap.Stringer("table", oldTable.Info.Name), zap.Stringer("db", oldTable.DB.Name))
			// get the the number of rows of each partition
			if tbl.OldTable.Info.Partition != nil {
				for _, oldDef := range tbl.OldTable.Info.Partition.Definitions {
					files := tbl.OldTable.FilesOfPhysicals[oldDef.ID]
					if len(files) > 0 {
						totalKvs := uint64(0)
						for _, file := range files {
							totalKvs += file.TotalKvs
						}
						// the total kvs contains the index kvs, but the stats meta needs the count of rows
						count := int64(totalKvs / uint64(len(oldTable.Info.Indices)+1))
						modifyCount := int64(0)
						if autoAnalyze {
							modifyCount = count
						}
						newDefID, err := utils.GetPartitionByName(tbl.Table, oldDef.Name)
						if err != nil {
							log.Error("failed to get the partition by name",
								zap.String("db name", tbl.OldTable.DB.Name.O),
								zap.String("table name", tbl.Table.Name.O),
								zap.String("partition name", oldDef.Name.O),
								zap.Int64("downstream table id", tbl.Table.ID),
								zap.Int64("upstream partition id", oldDef.ID),
							)
							return errors.Trace(err)
						}
						if statsErr = statsHandler.SaveMetaToStorage("br restore", false, statstypes.MetaUpdate{
							PhysicalID:  newDefID,
							Count:       count,
							ModifyCount: modifyCount,
						}); statsErr != nil {
							log.Error("update stats meta failed", zap.Int64("downstream table id", tbl.Table.ID), zap.Int64("downstream partition id", newDefID), zap.Error(statsErr))
						}
					}
				}
			}
			// the total kvs contains the index kvs, but the stats meta needs the count of rows
			count := int64(oldTable.TotalKvs / uint64(len(oldTable.Info.Indices)+1))
			modifyCount := int64(0)
			if autoAnalyze {
				modifyCount = count
			}
			if statsErr = statsHandler.SaveMetaToStorage("br restore", false, statstypes.MetaUpdate{
				PhysicalID:  tbl.Table.ID,
				Count:       count,
				ModifyCount: modifyCount,
			}); statsErr != nil {
				log.Error("update stats meta failed", zap.Int64("downstream table id", tbl.Table.ID), zap.Error(statsErr))
			}
		}
		updateCh.Inc()
		return nil
	}, func() {
		log.Info("all stats updated")
	})
	return outCh
}

func (rc *SnapClient) GoWaitTiFlashReady(
	ctx context.Context,
	inCh <-chan *CreatedTable,
	updateCh glue.Progress,
	errCh chan<- error,
) chan *CreatedTable {
	log.Info("Start to wait tiflash replica sync")
	outCh := defaultOutputTableChan()
	workers := tidbutil.NewWorkerPool(4, "WaitForTiflashReady")
	// TODO support tiflash store changes
	tikvStats, err := infosync.GetTiFlashStoresStat(context.Background())
	if err != nil {
		errCh <- err
	}
	tiFlashStores := make(map[int64]pdhttp.StoreInfo)
	for _, store := range tikvStats.Stores {
		if engine.IsTiFlashHTTPResp(&store.Store) {
			tiFlashStores[store.Store.ID] = store
		}
	}
	go concurrentHandleTablesCh(ctx, inCh, outCh, errCh, workers, func(c context.Context, tbl *CreatedTable) error {
		if tbl.Table != nil && tbl.Table.TiFlashReplica == nil {
			log.Info("table has no tiflash replica",
				zap.Stringer("table", tbl.OldTable.Info.Name),
				zap.Stringer("db", tbl.OldTable.DB.Name))
			updateCh.Inc()
			return nil
		}
		if rc.dom == nil {
			// unreachable, current we have initial domain in mgr.
			log.Fatal("unreachable, domain is nil")
		}
		log.Info("table has tiflash replica, start sync..",
			zap.Stringer("table", tbl.OldTable.Info.Name),
			zap.Stringer("db", tbl.OldTable.DB.Name))
		for {
			var progress float64
			if pi := tbl.Table.GetPartitionInfo(); pi != nil && len(pi.Definitions) > 0 {
				for _, p := range pi.Definitions {
					progressOfPartition, err := infosync.MustGetTiFlashProgress(p.ID, tbl.Table.TiFlashReplica.Count, &tiFlashStores)
					if err != nil {
						log.Warn("failed to get progress for tiflash partition replica, retry it",
							zap.Int64("tableID", tbl.Table.ID), zap.Int64("partitionID", p.ID), zap.Error(err))
						time.Sleep(time.Second)
						continue
					}
					progress += progressOfPartition
				}
				progress = progress / float64(len(pi.Definitions))
			} else {
				var err error
				progress, err = infosync.MustGetTiFlashProgress(tbl.Table.ID, tbl.Table.TiFlashReplica.Count, &tiFlashStores)
				if err != nil {
					log.Warn("failed to get progress for tiflash replica, retry it",
						zap.Int64("tableID", tbl.Table.ID), zap.Error(err))
					time.Sleep(time.Second)
					continue
				}
			}
			// check until progress is 1
			if progress == 1 {
				log.Info("tiflash replica synced",
					zap.Stringer("table", tbl.OldTable.Info.Name),
					zap.Stringer("db", tbl.OldTable.DB.Name))
				break
			}
			// just wait for next check
			// tiflash check the progress every 2s
			// we can wait 2.5x times
			time.Sleep(5 * time.Second)
		}
		updateCh.Inc()
		return nil
	}, func() {
		log.Info("all tiflash replica synced")
	})
	return outCh
}
