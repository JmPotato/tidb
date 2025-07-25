// Copyright 2022 PingCAP, Inc.
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

package ttlworker

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/infoschema"
	infoschemacontext "github.com/pingcap/tidb/pkg/infoschema/context"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/parser/terror"
	"github.com/pingcap/tidb/pkg/session/syssession"
	"github.com/pingcap/tidb/pkg/sessionctx/vardef"
	timerapi "github.com/pingcap/tidb/pkg/timer/api"
	ttltablestore "github.com/pingcap/tidb/pkg/timer/tablestore"
	"github.com/pingcap/tidb/pkg/ttl/cache"
	"github.com/pingcap/tidb/pkg/ttl/client"
	"github.com/pingcap/tidb/pkg/ttl/metrics"
	"github.com/pingcap/tidb/pkg/ttl/session"
	"github.com/pingcap/tidb/pkg/util/intest"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"github.com/pingcap/tidb/pkg/util/timeutil"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/multierr"
	"go.uber.org/zap"
)

const scanTaskNotificationType string = "scan"

const insertNewTableIntoStatusTemplate = "INSERT INTO mysql.tidb_ttl_table_status (table_id,parent_table_id) VALUES (%?, %?)"
const setTableStatusOwnerTemplate = `UPDATE mysql.tidb_ttl_table_status
	SET current_job_id = %?,
		current_job_owner_id = %?,
		current_job_start_time = %?,
		current_job_status = 'running',
		current_job_status_update_time = %?,
		current_job_ttl_expire = %?,
		current_job_owner_hb_time = %?
	WHERE table_id = %?`
const updateHeartBeatTemplate = "UPDATE mysql.tidb_ttl_table_status SET current_job_owner_hb_time = %? WHERE table_id = %? AND current_job_owner_id = %?"
const taskGCTemplate = `DELETE task FROM
		mysql.tidb_ttl_task task
	left join
		mysql.tidb_ttl_table_status job
	ON task.job_id = job.current_job_id
	WHERE job.table_id IS NULL`

const ttlJobHistoryGCTemplate = `DELETE FROM mysql.tidb_ttl_job_history WHERE create_time < CURDATE() - INTERVAL 90 DAY`

// don't need to consider heartbeat timeout now, because the job will only be GCed when there is no owner and no job running
const ttlTableStatusGCWithoutIDTemplate = `DELETE FROM mysql.tidb_ttl_table_status WHERE current_job_status IS NULL`

var timeFormat = time.DateTime

func insertNewTableIntoStatusSQL(tableID int64, parentTableID int64) (string, []any) {
	return insertNewTableIntoStatusTemplate, []any{tableID, parentTableID}
}

func setTableStatusOwnerSQL(uuid string, tableID int64, jobStart time.Time, now time.Time, currentJobTTLExpire time.Time, id string) (string, []any) {
	return setTableStatusOwnerTemplate, []any{uuid, id, jobStart.Format(timeFormat), now.Format(timeFormat), currentJobTTLExpire.Format(timeFormat), now.Format(timeFormat), tableID}
}

func updateHeartBeatSQL(tableID int64, now time.Time, id string) (string, []any) {
	return updateHeartBeatTemplate, []any{now.Format(timeFormat), tableID, id}
}

func gcTTLTableStatusGCSQL(existIDs []int64) string {
	existIDStrs := make([]string, 0, len(existIDs))
	for _, id := range existIDs {
		existIDStrs = append(existIDStrs, strconv.Itoa(int(id)))
	}

	if len(existIDStrs) > 0 {
		return ttlTableStatusGCWithoutIDTemplate + fmt.Sprintf(` AND table_id NOT IN (%s)`, strings.Join(existIDStrs, ","))
	}
	return ttlTableStatusGCWithoutIDTemplate
}

// JobManager schedules and manages the ttl jobs on this instance
type JobManager struct {
	// the `runningJobs`, `scanWorkers` and `delWorkers` should be protected by mutex:
	// `runningJobs` will be shared in the state loop and schedule loop
	// `scanWorkers` and `delWorkers` can be modified by setting variables at any time
	baseWorker

	sessPool syssession.Pool

	// id is the ddl id of this instance
	id string

	store           kv.Storage
	cmdCli          client.CommandClient
	notificationCli client.NotificationClient
	etcd            *clientv3.Client

	// infoSchemaCache and tableStatusCache are a cache stores the information from info schema and the tidb_ttl_table_status
	// table. They don't need to be protected by mutex, because they are only used in job loop goroutine.
	infoSchemaCache  *cache.InfoSchemaCache
	tableStatusCache *cache.TableStatusCache

	// runningJobs record all ttlJob waiting in local
	// when a job for a table is created, it could spawn several scan tasks. If there are too many scan tasks, and they cannot
	// be fully consumed by local scan workers, their states should be recorded in the runningJobs, so that we could continue
	// to poll scan tasks from the job in the future when there are scan workers in idle.
	runningJobs []*ttlJob

	taskManager *taskManager

	lastReportDelayMetricsTime time.Time
	leaderFunc                 func() bool
}

// NewJobManager creates a new ttl job manager
func NewJobManager(id string, sessPool syssession.Pool, store kv.Storage, etcdCli *clientv3.Client, leaderFunc func() bool) (manager *JobManager) {
	manager = &JobManager{}
	manager.id = id
	manager.store = store
	manager.sessPool = sessPool

	manager.init(manager.jobLoop)
	manager.ctx = logutil.WithKeyValue(manager.ctx, "ttl-worker", "job-manager")
	if intest.InTest {
		// in test environment, in the same log there will be multiple ttl managers, so we need to distinguish them
		manager.ctx = logutil.WithKeyValue(manager.ctx, "ttl-worker", id)
	}

	manager.infoSchemaCache = cache.NewInfoSchemaCache(getUpdateInfoSchemaCacheInterval())
	manager.tableStatusCache = cache.NewTableStatusCache(getUpdateTTLTableStatusCacheInterval())

	if etcdCli != nil {
		manager.cmdCli = client.NewCommandClient(etcdCli)
		manager.notificationCli = client.NewNotificationClient(etcdCli)
		manager.etcd = etcdCli
	} else {
		manager.cmdCli = client.NewMockCommandClient()
		manager.notificationCli = client.NewMockNotificationClient()
	}

	manager.taskManager = newTaskManager(manager.ctx, sessPool, manager.infoSchemaCache, id, store)
	manager.leaderFunc = leaderFunc
	return
}

func (m *JobManager) isLeader() bool {
	return m.leaderFunc != nil && m.leaderFunc()
}

func (m *JobManager) jobLoop() error {
	defer func() {
		logutil.Logger(m.ctx).Info("ttlJobManager loop exited.")
	}()
	return withSession(m.sessPool, m.jobLoopWithSession)
}

func (m *JobManager) jobLoopWithSession(se session.Session) (err error) {
	timerStore := ttltablestore.NewTableTimerStore(1, m.sessPool, "mysql", "tidb_timers", m.etcd)
	jobRequestCh := make(chan *SubmitTTLManagerJobRequest)
	adapter := NewManagerJobAdapter(m.store, m.sessPool, jobRequestCh)
	timerRT := newTTLTimerRuntime(timerStore, adapter)
	timerSyncer := NewTTLTimerSyncer(m.sessPool, timerapi.NewDefaultTimerClient(timerStore))
	defer func() {
		timerRT.Pause()
		timerStore.Close()
		err = multierr.Combine(err, multierr.Combine(m.taskManager.resizeScanWorkers(0), m.taskManager.resizeDelWorkers(0)))
		logutil.Logger(m.ctx).Info("ttlJobManager loop exited.")
	}()

	infoSchemaCacheUpdateTicker := time.Tick(m.infoSchemaCache.GetInterval())
	tableStatusCacheUpdateTicker := time.Tick(m.tableStatusCache.GetInterval())
	resizeWorkersTicker := time.Tick(getResizeWorkersInterval())
	gcTicker := time.Tick(getTTLGCInterval())

	scheduleJobTicker := time.Tick(getCheckJobInterval())
	jobCheckTicker := time.Tick(getCheckJobInterval())
	updateJobHeartBeatTicker := time.Tick(getHeartbeatInterval())
	timerTicker := time.Tick(getJobManagerLoopSyncTimerInterval())

	scheduleTaskTicker := time.Tick(getTaskManagerLoopTickerInterval())
	updateTaskHeartBeatTicker := time.Tick(getTaskManagerHeartBeatInterval())
	taskCheckTicker := time.Tick(getTaskManagerLoopCheckTaskInterval())
	checkScanTaskFinishedTicker := time.Tick(getTaskManagerLoopTickerInterval())

	cmdWatcher := m.cmdCli.WatchCommand(m.ctx)
	scanTaskNotificationWatcher := m.notificationCli.WatchNotification(m.ctx, scanTaskNotificationType)
	m.taskManager.resizeWorkersWithSysVar()
	for {
		m.reportMetrics(se)
		m.taskManager.reportMetrics()
		now := se.Now()

		select {
		// misc
		case <-m.ctx.Done():
			return nil
		case <-timerTicker:
			m.onTimerTick(se, timerRT, timerSyncer, now)
		case jobReq := <-jobRequestCh:
			m.handleSubmitJobRequest(se, jobReq)
		case <-infoSchemaCacheUpdateTicker:
			err := m.updateInfoSchemaCache(se)
			if err != nil {
				logutil.Logger(m.ctx).Warn("fail to update info schema cache", zap.Error(err))
			}
		case <-tableStatusCacheUpdateTicker:
			err := m.updateTableStatusCache(se)
			if err != nil {
				logutil.Logger(m.ctx).Warn("fail to update table status cache", zap.Error(err))
			}
		case <-gcTicker:
			gcCtx, cancel := context.WithTimeout(m.ctx, ttlInternalSQLTimeout)
			m.DoGC(gcCtx, se, now)
			cancel()
		// Job Schedule loop:
		case <-updateJobHeartBeatTicker:
			updateHeartBeatCtx, cancel := context.WithTimeout(m.ctx, ttlInternalSQLTimeout)
			m.updateHeartBeat(updateHeartBeatCtx, se, now)
			cancel()
		case <-jobCheckTicker:
			m.checkFinishedJob(se)
			m.checkNotOwnJob()
		case <-scheduleJobTicker:
			m.rescheduleJobs(se, now)
		case cmd, ok := <-cmdWatcher:
			if !ok {
				if m.ctx.Err() != nil {
					return nil
				}

				logutil.BgLogger().Warn("The TTL cmd watcher is closed unexpectedly, re-watch it again")
				cmdWatcher = m.cmdCli.WatchCommand(m.ctx)
				continue
			}

			if triggerJobCmd, ok := cmd.GetTriggerTTLJobRequest(); ok {
				m.triggerTTLJob(cmd.RequestID, triggerJobCmd, se, timerStore)
			}

		// Task Manager Loop
		case <-scheduleTaskTicker:
			m.taskManager.rescheduleTasks(se, now)
		case _, ok := <-scanTaskNotificationWatcher:
			if !ok {
				if m.ctx.Err() != nil {
					return nil
				}

				logutil.BgLogger().Warn("The TTL scan task notification watcher is closed unexpectedly, re-watch it again")
				scanTaskNotificationWatcher = m.notificationCli.WatchNotification(m.ctx, scanTaskNotificationType)
				continue
			}
			m.taskManager.rescheduleTasks(se, now)
		case <-taskCheckTicker:
			m.taskManager.checkInvalidTask(se)
			m.taskManager.checkFinishedTask(se, now)
		case <-resizeWorkersTicker:
			m.taskManager.resizeWorkersWithSysVar()
		case <-updateTaskHeartBeatTicker:
			updateHeartBeatCtx, cancel := context.WithTimeout(m.ctx, ttlInternalSQLTimeout)
			m.taskManager.updateHeartBeat(updateHeartBeatCtx, se, now)
			cancel()
		case <-checkScanTaskFinishedTicker:
			if m.taskManager.handleScanFinishedTask() {
				m.taskManager.rescheduleTasks(se, now)
			}
		case <-m.taskManager.notifyStateCh:
			if m.taskManager.handleScanFinishedTask() {
				m.taskManager.rescheduleTasks(se, now)
			}
		}
	}
}

func (m *JobManager) onTimerTick(se session.Session, rt *ttlTimerRuntime, syncer *TTLTimersSyncer, now time.Time) {
	if !m.isLeader() {
		rt.Pause()
		syncer.Reset()
		return
	}

	rt.Resume()
	lastSyncTime, lastSyncVer := syncer.GetLastSyncInfo()
	sinceLastSync := now.Sub(lastSyncTime)
	minSyncDuration := 5 * time.Second
	if intest.InTest {
		// in test, we can set the minSyncDuration to 1ms to boost the test speed
		minSyncDuration = time.Millisecond
	}

	if sinceLastSync < minSyncDuration {
		// limit timer sync frequency by every 5 seconds
		return
	}

	is := se.GetLatestInfoSchema().(infoschema.InfoSchema)
	if is.SchemaMetaVersion() > lastSyncVer || sinceLastSync > 2*time.Minute {
		// only sync timer when information schema version upgraded, or it has not been synced for more than 2 minutes.
		syncer.SyncTimers(m.ctx, is)
	}
}

func (m *JobManager) handleSubmitJobRequest(se session.Session, jobReq *SubmitTTLManagerJobRequest) {
	if !m.isLeader() {
		jobReq.RespCh <- errors.Errorf("current TTL manager is not the leader")
		return
	}

	if err := m.updateInfoSchemaCache(se); err != nil {
		logutil.Logger(m.ctx).Warn("failed to update info schema cache", zap.Error(err))
	}

	tbl, ok := m.infoSchemaCache.Tables[jobReq.PhysicalID]
	if !ok {
		jobReq.RespCh <- errors.Errorf("physical id: %d not exists in information schema", jobReq.PhysicalID)
		return
	}

	if tbl.TableInfo.ID != jobReq.TableID {
		jobReq.RespCh <- errors.Errorf("table id '%d' != '%d' for physical table with id: %d", jobReq.TableID, tbl.TableInfo.ID, jobReq.PhysicalID)
		return
	}

	_, err := m.lockNewJob(m.ctx, se, tbl, se.Now(), jobReq.RequestID, false)
	jobReq.RespCh <- err
}

func (m *JobManager) triggerTTLJob(requestID string, cmd *client.TriggerNewTTLJobRequest, se session.Session, store *timerapi.TimerStore) {
	ok, err := m.cmdCli.TakeCommand(m.ctx, requestID)
	if err != nil {
		logutil.BgLogger().Error("failed to take TTL trigger job command",
			zap.String("requestID", requestID),
			zap.String("database", cmd.DBName),
			zap.String("table", cmd.TableName))
		return
	}

	if !ok {
		return
	}

	logutil.BgLogger().Info("Get a command to trigger a new TTL job",
		zap.String("requestID", requestID),
		zap.String("database", cmd.DBName),
		zap.String("table", cmd.TableName))

	responseErr := func(err error) {
		terror.Log(m.cmdCli.ResponseCommand(m.ctx, requestID, err))
	}

	if !vardef.EnableTTLJob.Load() {
		responseErr(errors.New("tidb_ttl_job_enable is disabled"))
		return
	}

	if !timeutil.WithinDayTimePeriod(vardef.TTLJobScheduleWindowStartTime.Load(), vardef.TTLJobScheduleWindowEndTime.Load(), se.Now()) {
		responseErr(errors.New("not in TTL job window"))
		return
	}

	if err = m.infoSchemaCache.Update(se); err != nil {
		responseErr(err)
		return
	}

	if err = m.tableStatusCache.Update(m.ctx, se); err != nil {
		responseErr(err)
		return
	}

	var tables []*cache.PhysicalTable
	for _, tbl := range m.infoSchemaCache.Tables {
		if tbl.Schema.L == strings.ToLower(cmd.DBName) && tbl.Name.L == strings.ToLower(cmd.TableName) {
			tables = append(tables, tbl)
		}
	}

	if len(tables) == 0 {
		responseErr(errors.Errorf("table %s.%s not exists", cmd.DBName, cmd.TableName))
		return
	}

	syncer := NewTTLTimerSyncer(m.sessPool, timerapi.NewDefaultTimerClient(store))
	tableResults := make([]*client.TriggerNewTTLJobTableResult, 0, len(tables))
	getJobFns := make([]func() (string, bool, error), 0, len(tables))
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Minute)
	for _, tbl := range tables {
		tblResult := &client.TriggerNewTTLJobTableResult{
			TableID:       tbl.ID,
			DBName:        cmd.DBName,
			TableName:     cmd.TableName,
			PartitionName: tbl.Partition.O,
		}
		tableResults = append(tableResults, tblResult)

		fn, err := syncer.ManualTriggerTTLTimer(m.ctx, tbl)
		if err != nil {
			tblResult.ErrorMessage = err.Error()
			getJobFns = append(getJobFns, nil)
		} else {
			getJobFns = append(getJobFns, fn)
		}
	}

	go func() {
		defer cancel()
		ticker := time.NewTicker(getCheckJobTriggeredInterval())
		defer ticker.Stop()
	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case <-ticker.C:
			}

			for i := range getJobFns {
				fn := getJobFns[i]
				if fn == nil {
					continue
				}

				jobID, ok, err := fn()
				if err != nil {
					tableResults[i].ErrorMessage = err.Error()
					getJobFns[i] = nil
					continue
				}

				if ok {
					tableResults[i].JobID = jobID
					getJobFns[i] = nil
				}
			}

			for _, fn := range getJobFns {
				if fn != nil {
					continue loop
				}
			}
			break
		}

		allError := true
		for _, r := range tableResults {
			if r.JobID == "" && r.ErrorMessage == "" {
				r.ErrorMessage = "timeout"
			}

			if r.ErrorMessage == "" {
				allError = false
			}
		}

		if allError {
			responseErr(errors.New(tableResults[0].ErrorMessage))
			return
		}

		terror.Log(m.cmdCli.ResponseCommand(m.ctx, requestID, &client.TriggerNewTTLJobResponse{
			TableResult: tableResults,
		}))

		tableResultsJSON, _ := json.Marshal(tableResults)
		logutil.BgLogger().Info("Done to trigger a new TTL job",
			zap.String("requestID", requestID),
			zap.String("database", cmd.DBName),
			zap.String("table", cmd.TableName),
			zap.ByteString("tableResults", tableResultsJSON),
		)
	}()
}

func (m *JobManager) reportMetrics(se session.Session) {
	var runningJobs, cancellingJobs float64
	for _, job := range m.runningJobs {
		switch job.status {
		case cache.JobStatusRunning:
			runningJobs++
		case cache.JobStatusCancelling:
			cancellingJobs++
		}
	}
	metrics.RunningJobsCnt.Set(runningJobs)
	metrics.CancellingJobsCnt.Set(cancellingJobs)

	if !m.isLeader() {
		// only the leader can do collect delay metrics to reduce the performance overhead
		metrics.ClearDelayMetrics()
		return
	}

	if time.Since(m.lastReportDelayMetricsTime) > 10*time.Minute {
		m.lastReportDelayMetricsTime = time.Now()
		logutil.Logger(m.ctx).Info("TTL leader to collect delay metrics")
		records, err := GetDelayMetricRecords(m.ctx, se, time.Now())
		if err != nil {
			logutil.Logger(m.ctx).Info("failed to get TTL delay metrics", zap.Error(err))
		} else {
			metrics.UpdateDelayMetrics(records)
		}
	}
}

// checkNotOwnJob removes the job whose current job owner is not yourself
func (m *JobManager) checkNotOwnJob() {
	// reverse iteration so that we could remove the job safely in the loop
	for i := len(m.runningJobs) - 1; i >= 0; i-- {
		job := m.runningJobs[i]

		tableStatus := m.tableStatusCache.Tables[job.tableID]
		if tableStatus == nil || tableStatus.CurrentJobOwnerID != m.id {
			logger := logutil.Logger(m.ctx).With(zap.String("jobID", job.id))
			if tableStatus != nil {
				logger.With(zap.String("newJobOwnerID", tableStatus.CurrentJobOwnerID))
			}
			logger.Info("job has been taken over by another node")
			m.removeJob(job)
		}
	}
}

func (m *JobManager) findAllTasksForJob(se session.Session, jobID string) ([]*cache.TTLTask, error) {
	timeoutJobCtx, cancel := context.WithTimeout(m.ctx, ttlInternalSQLTimeout)
	defer cancel()

	sql, args := cache.SelectFromTTLTaskWithJobID(jobID)
	rows, err := se.ExecuteSQL(timeoutJobCtx, sql, args...)
	cancel()
	if err != nil {
		logutil.Logger(m.ctx).Warn("fail to execute sql", zap.String("sql", sql), zap.Any("args", args), zap.Error(err))
		return nil, err
	}

	allTasks := make([]*cache.TTLTask, 0, len(rows))
	for _, r := range rows {
		task, err := cache.RowToTTLTask(se.GetSessionVars().Location(), r)
		if err != nil {
			logutil.Logger(m.ctx).Warn("fail to read task", zap.Error(err), zap.String("jobID", jobID))
			return nil, err
		}
		allTasks = append(allTasks, task)
	}

	return allTasks, nil
}

func (m *JobManager) checkFinishedJob(se session.Session) {
	// reverse iteration so that we could remove the job safely in the loop
	for i := len(m.runningJobs) - 1; i >= 0; i-- {
		job := m.runningJobs[i]
		allTasks, err := m.findAllTasksForJob(se, job.id)
		if err != nil {
			logutil.Logger(m.ctx).Warn("fail to find all tasks for job. Skip check finished", zap.String("jobID", job.id), zap.Error(err))
			continue
		}

		allFinished := true
		for _, task := range allTasks {
			if task.Status != cache.TaskStatusFinished {
				allFinished = false
				break
			}
		}

		if allFinished {
			logger := m.jobLogger(job)
			logger.Info("job has finished")
			summary, err := summarizeTaskResultWithError(allTasks, nil)
			if err != nil {
				logger.Info("fail to summarize job", zap.Error(err))
			}
			logger.Info("job has finished", zap.String("summary", summary.SummaryText),
				zap.Uint64("totalRows", summary.TotalRows), zap.Uint64("successRows", summary.SuccessRows), zap.Uint64("errorRows", summary.ErrorRows),
				zap.String("scanTaskError", summary.ScanTaskErr))
			err = job.finish(se, se.Now(), summary)
			if err != nil {
				logger.Warn("fail to finish job", zap.Error(err))
				continue
			}
			m.removeJob(job)
		}
	}
}

func (m *JobManager) rescheduleJobs(se session.Session, now time.Time) {
	// Try to lock HB timeout jobs, to avoid the case that when the `tidb_ttl_job_enable = 'OFF'`, the HB timeout job will
	// never be cancelled.
	jobTables := m.readyForLockHBTimeoutJobTables(now)
	// TODO: also consider to resume tables, but it's fine to left them there, as other nodes will take this job
	// when the heart beat is not sent
	for _, table := range jobTables {
		logger := logutil.Logger(m.ctx).With(
			zap.Int64("tableID", table.TableID),
		)
		logger.Info("try lock new job")
		if _, err := m.lockHBTimeoutJob(m.ctx, se, table.TableID, table.ParentTableID, now); err != nil {
			logger.Warn("failed to lock heartbeat timeout job", zap.Error(err))
		}
	}

	cancelJobs := false
	cancelReason := ""
	switch {
	case !vardef.EnableTTLJob.Load():
		cancelJobs = true
		cancelReason = "tidb_ttl_job_enable turned off"
	case !timeutil.WithinDayTimePeriod(vardef.TTLJobScheduleWindowStartTime.Load(), vardef.TTLJobScheduleWindowEndTime.Load(), now):
		cancelJobs = true
		cancelReason = "out of TTL job schedule window"
	}

	if cancelJobs {
		if len(m.runningJobs) > 0 {
			// reverse iteration so that we could remove the job safely in the loop
			for i := len(m.runningJobs) - 1; i >= 0; i-- {
				job := m.runningJobs[i]

				logger := m.jobLogger(job)
				logger.Info(fmt.Sprintf("cancel job because %s", cancelReason))
				allTasks, err := m.findAllTasksForJob(se, job.id)
				if err != nil {
					logger.Warn("fail to find all tasks for job. Summarize nothing for cancel job", zap.String("jobID", job.id), zap.Error(err))
				}
				summary, err := summarizeTaskResultWithError(allTasks, errors.New(cancelReason))
				if err != nil {
					logger.Warn("fail to summarize job", zap.Error(err))
				}
				err = job.finish(se, now, summary)
				if err != nil {
					logger.Warn("fail to finish job", zap.Error(err))
					continue
				}
				m.removeJob(job)
			}
		}
		return
	}

	// if the table of a running job disappears, also cancel it
	// reverse iteration so that we could remove the job safely in the loop
	for i := len(m.runningJobs) - 1; i >= 0; i-- {
		job := m.runningJobs[i]

		_, ok := m.infoSchemaCache.Tables[job.tableID]
		if ok {
			continue
		}

		// when the job is locked, it can be found in `infoSchemaCache`. Therefore, it must have been dropped.
		logger := m.jobLogger(job)
		logger.Info("cancel job because the table has been dropped or it's no longer TTL table")
		allTasks, err := m.findAllTasksForJob(se, job.id)
		if err != nil {
			logger.Warn("fail to find all tasks for job. Summarize nothing for cancel job", zap.String("jobID", job.id), zap.Error(err))
		}
		summary, err := summarizeTaskResultWithError(allTasks, errors.New("TTL table has been removed or the TTL on this table has been stopped"))
		if err != nil {
			logger.Warn("fail to summarize job", zap.Error(err))
		}
		err = job.finish(se, now, summary)
		if err != nil {
			logger.Warn("fail to finish job", zap.Error(err))
			continue
		}
		m.removeJob(job)
	}
}

func (m *JobManager) localJobs() []*ttlJob {
	jobs := make([]*ttlJob, 0, len(m.runningJobs))
	for _, job := range m.runningJobs {
		status := m.tableStatusCache.Tables[job.tableID]
		if status == nil || status.CurrentJobOwnerID != m.id {
			// these jobs will be removed in `checkNotOwnJob`
			continue
		}

		jobs = append(jobs, job)
	}
	return jobs
}

// readyForLockHBTimeoutJobTables returns all tables whose job is timeout and should be taken over
func (m *JobManager) readyForLockHBTimeoutJobTables(now time.Time) []*cache.TableStatus {
	tables := make([]*cache.TableStatus, 0, len(m.infoSchemaCache.Tables))
tblLoop:
	for _, status := range m.tableStatusCache.Tables {
		// If this node already has a job for this table, just ignore.
		// Actually, the logic should ensure this condition never meet, we still add the check here to keep safety
		// (especially when the content of the status table is incorrect)
		for _, job := range m.runningJobs {
			if job.tableID == status.TableID {
				continue tblLoop
			}
		}

		if m.couldLockJobForExistJob(status, now) {
			tables = append(tables, status)
		}
	}

	return tables
}

// couldLockJobForCreate returns whether a table should be tried to create a new TTL job
func (m *JobManager) couldLockJobForCreate(tableStatus *cache.TableStatus, table *cache.PhysicalTable, now time.Time, checkScheduleInterval bool) bool {
	if tableStatus == nil {
		return true
	}

	if tableStatus.CurrentJobID != "" {
		return false
	}

	if !checkScheduleInterval {
		return true
	}

	startTime := tableStatus.LastJobStartTime

	interval, err := table.TTLInfo.GetJobInterval()
	if err != nil {
		logutil.Logger(m.ctx).Warn(
			"illegal job interval",
			zap.Error(err),
			zap.Int64("tableID", table.ID),
			zap.String("table", table.FullName()),
		)
		return false
	}
	return startTime.Add(interval).Before(now)
}

// couldLockJob returns whether a job for a table should be taken over.
func (m *JobManager) couldLockJobForExistJob(tableStatus *cache.TableStatus, now time.Time) bool {
	// if isCreate is false, it means to take over an exist job
	if tableStatus == nil || tableStatus.CurrentJobID == "" {
		return false
	}

	if tableStatus.CurrentJobOwnerID != "" {
		// see whether it's heart beat time is expired
		hbTime := tableStatus.CurrentJobOwnerHBTime
		// jobManagerLoopTickerInterval is used to do heartbeat periodically.
		// Use twice the time to detect the heartbeat timeout.
		hbTimeout := getHeartbeatInterval() * 2
		if interval := getUpdateTTLTableStatusCacheInterval() * 2; interval > hbTimeout {
			// tableStatus is get from the cache which may contain stale data.
			// So if cache update interval > heartbeat interval, use the cache update interval instead.
			hbTimeout = interval
		}
		if hbTime.Add(hbTimeout).Before(now) {
			logutil.Logger(m.ctx).Info("job heartbeat has stopped",
				zap.String("jobID", tableStatus.CurrentJobID),
				zap.Int64("tableID", tableStatus.TableID),
				zap.Time("hbTime", hbTime),
				zap.Time("now", now),
			)
			return true
		}
		return false
	}
	return true
}

func (m *JobManager) lockHBTimeoutJob(ctx context.Context, se session.Session, tableID int64, parentTableID int64, now time.Time) (*ttlJob, error) {
	var jobID string
	var jobStart time.Time
	var expireTime time.Time
	err := se.RunInTxn(ctx, func() error {
		tableStatus, err := m.getTableStatusForUpdateNotWait(ctx, se, tableID, parentTableID, false)
		if err != nil {
			return err
		}

		if tableStatus == nil || !m.couldLockJobForExistJob(tableStatus, now) {
			return errors.Errorf("couldn't lock timeout TTL job for table id '%d'", tableID)
		}

		jobID = tableStatus.CurrentJobID
		jobStart = tableStatus.CurrentJobStartTime
		expireTime = tableStatus.CurrentJobTTLExpire
		intest.Assert(se.GetSessionVars().TimeZone.String() == now.Location().String())
		sql, args := setTableStatusOwnerSQL(tableStatus.CurrentJobID, tableID, jobStart, now, expireTime, m.id)
		if _, err = se.ExecuteSQL(ctx, sql, args...); err != nil {
			return errors.Wrapf(err, "execute sql: %s", sql)
		}
		return nil
	}, session.TxnModePessimistic)

	if err != nil {
		return nil, err
	}

	return m.appendLockedJob(jobID, se, jobStart, expireTime, tableID)
}

// lockNewJob locks a new job
func (m *JobManager) lockNewJob(ctx context.Context, se session.Session, table *cache.PhysicalTable, now time.Time, jobID string, checkScheduleInterval bool) (*ttlJob, error) {
	var expireTime time.Time
	err := se.RunInTxn(ctx, func() error {
		tableStatus, err := m.getTableStatusForUpdateNotWait(ctx, se, table.ID, table.TableInfo.ID, true)
		if err != nil {
			return err
		}

		if !m.couldLockJobForCreate(tableStatus, m.infoSchemaCache.Tables[tableStatus.TableID], now, checkScheduleInterval) {
			return errors.New("couldn't schedule ttl job")
		}

		expireTime, err = table.EvalExpireTime(m.ctx, se, now)
		if err != nil {
			return err
		}

		intest.Assert(se.GetSessionVars().TimeZone.String() == now.Location().String())
		intest.Assert(se.GetSessionVars().TimeZone.String() == expireTime.Location().String())

		sql, args := setTableStatusOwnerSQL(jobID, table.ID, now, now, expireTime, m.id)
		_, err = se.ExecuteSQL(ctx, sql, args...)
		if err != nil {
			return errors.Wrapf(err, "execute sql: %s", sql)
		}

		sql, args = createJobHistorySQL(jobID, table, expireTime, now)
		_, err = se.ExecuteSQL(ctx, sql, args...)
		if err != nil {
			return errors.Wrapf(err, "execute sql: %s", sql)
		}

		ranges, err := table.SplitScanRanges(ctx, m.store, getScanSplitCnt(se.GetStore()))
		if err != nil {
			return errors.Wrap(err, "split scan ranges")
		}
		for scanID, r := range ranges {
			sql, args, err = cache.InsertIntoTTLTask(se.GetSessionVars().Location(), jobID, table.ID, scanID, r.Start, r.End, expireTime, now)
			if err != nil {
				return errors.Wrap(err, "encode scan task")
			}
			_, err = se.ExecuteSQL(ctx, sql, args...)
			if err != nil {
				return errors.Wrapf(err, "execute sql: %s", sql)
			}
		}

		return nil
	}, session.TxnModePessimistic)
	if err != nil {
		return nil, err
	}

	return m.appendLockedJob(jobID, se, now, expireTime, table.ID)
}

func (m *JobManager) getTableStatusForUpdateNotWait(ctx context.Context, se session.Session, physicalID int64, parentTableID int64, createIfNotExist bool) (*cache.TableStatus, error) {
	sql, args := cache.SelectFromTTLTableStatusWithID(physicalID)
	// use ` FOR UPDATE NOWAIT`, then if the new job has been locked by other nodes, it will return:
	// [tikv:3572]Statement aborted because lock(s) could not be acquired immediately and NOWAIT is set.
	// Then this tidb node will not waste resource in calculating the ranges.
	rows, err := se.ExecuteSQL(ctx, sql+" FOR UPDATE NOWAIT", args...)
	if err != nil {
		return nil, errors.Wrapf(err, "execute sql: %s", sql)
	}

	if len(rows) == 0 && !createIfNotExist {
		return nil, nil
	}

	if len(rows) == 0 {
		// cannot find the row, insert the status row
		sql, args = insertNewTableIntoStatusSQL(physicalID, parentTableID)
		_, err = se.ExecuteSQL(ctx, sql, args...)
		if err != nil {
			return nil, errors.Wrapf(err, "execute sql: %s", sql)
		}

		sql, args = cache.SelectFromTTLTableStatusWithID(physicalID)
		rows, err = se.ExecuteSQL(ctx, sql+" FOR UPDATE NOWAIT", args...)
		if err != nil {
			return nil, errors.Wrapf(err, "execute sql: %s", sql)
		}

		if len(rows) == 0 {
			return nil, errors.New("table status row still doesn't exist after insertion")
		}
	}

	return cache.RowToTableStatus(se.GetSessionVars().Location(), rows[0])
}

func (m *JobManager) appendLockedJob(id string, se session.Session, createTime time.Time, expireTime time.Time, tableID int64) (*ttlJob, error) {
	// successfully update the table status, will need to refresh the cache.
	err := m.updateInfoSchemaCache(se)
	if err != nil {
		return nil, err
	}
	err = m.updateTableStatusCache(se)
	if err != nil {
		return nil, err
	}

	logger := logutil.Logger(m.ctx).With(
		zap.String("jobID", id),
		zap.Int64("tableID", tableID),
	)

	// job is created, notify every scan managers to fetch new tasks
	err = m.notificationCli.Notify(m.ctx, scanTaskNotificationType, id)
	if err != nil {
		logger.Warn("fail to trigger scan tasks", zap.Error(err))
	}

	job := &ttlJob{
		id:      id,
		ownerID: m.id,

		createTime:    createTime,
		assignTime:    time.Now(),
		ttlExpireTime: expireTime,
		tableID:       tableID,

		status: cache.JobStatusRunning,
	}
	logger = m.jobLogger(job)

	logger.Info("append new running job")
	m.appendJob(job)

	return job, nil
}

// updateHeartBeat updates the heartbeat for all task with current instance as owner
func (m *JobManager) updateHeartBeat(ctx context.Context, se session.Session, now time.Time) {
	for _, job := range m.localJobs() {
		err := m.updateHeartBeatForJob(ctx, se, now, job)
		if err != nil {
			m.jobLogger(job).Warn("fail to update heartbeat for job",
				zap.Error(err))
		}
	}
}

func (m *JobManager) updateHeartBeatForJob(ctx context.Context, se session.Session, now time.Time, job *ttlJob) error {
	if job.createTime.Add(ttlJobTimeout).Before(now) {
		m.jobLogger(job).Info("job is timeout")
		tasks, err := m.findAllTasksForJob(se, job.id)
		if err != nil {
			m.jobLogger(job).Warn("fail to find all tasks for job. Summarize nothing for timeout job",
				zap.String("jobID", job.id), zap.Error(err))
		}
		summary, err := summarizeTaskResultWithError(tasks, errors.New("job is timeout"))
		if err != nil {
			return errors.Wrapf(err, "fail to summarize job")
		}
		err = job.finish(se, now, summary)
		if err != nil {
			return errors.Wrapf(err, "fail to finish job")
		}
		m.removeJob(job)
		return nil
	}

	intest.Assert(se.GetSessionVars().TimeZone.String() == now.Location().String())
	sql, args := updateHeartBeatSQL(job.tableID, now, m.id)
	_, err := se.ExecuteSQL(ctx, sql, args...)
	if err != nil {
		return errors.Wrapf(err, "execute sql: %s", sql)
	}

	if se.GetSessionVars().StmtCtx.AffectedRows() != 1 {
		return errors.Errorf("fail to update job heartbeat, maybe the owner is not myself (%s), affected rows: %d",
			m.id, se.GetSessionVars().StmtCtx.AffectedRows())
	}

	return nil
}

// updateInfoSchemaCache updates the cache of information schema
func (m *JobManager) updateInfoSchemaCache(se session.Session) error {
	return m.infoSchemaCache.Update(se)
}

// updateTableStatusCache updates the cache of table status
func (m *JobManager) updateTableStatusCache(se session.Session) error {
	cacheUpdateCtx, cancel := context.WithTimeout(m.ctx, ttlInternalSQLTimeout)
	defer cancel()
	return m.tableStatusCache.Update(cacheUpdateCtx, se)
}

func (m *JobManager) removeJob(finishedJob *ttlJob) {
	for idx, job := range m.runningJobs {
		if job.id == finishedJob.id {
			if idx+1 < len(m.runningJobs) {
				m.runningJobs = append(m.runningJobs[0:idx], m.runningJobs[idx+1:]...)
			} else {
				m.runningJobs = m.runningJobs[0:idx]
			}
			return
		}
	}
}

func (m *JobManager) appendJob(job *ttlJob) {
	m.runningJobs = append(m.runningJobs, job)
}

// GetCommandCli returns the command client
func (m *JobManager) GetCommandCli() client.CommandClient {
	return m.cmdCli
}

// GetNotificationCli returns the notification client
func (m *JobManager) GetNotificationCli() client.NotificationClient {
	return m.notificationCli
}

// TTLSummary is the summary for TTL job
type TTLSummary struct {
	TotalRows   uint64 `json:"total_rows"`
	SuccessRows uint64 `json:"success_rows"`
	ErrorRows   uint64 `json:"error_rows"`

	TotalScanTask     int `json:"total_scan_task"`
	ScheduledScanTask int `json:"scheduled_scan_task"`
	FinishedScanTask  int `json:"finished_scan_task"`

	ScanTaskErr string `json:"scan_task_err,omitempty"`
	SummaryText string `json:"-"`
}

func summarizeTaskResultWithError(tasks []*cache.TTLTask, err error) (*TTLSummary, error) {
	summary := &TTLSummary{}
	allErr := err
	for _, t := range tasks {
		if t.State != nil {
			summary.TotalRows += t.State.TotalRows
			summary.SuccessRows += t.State.SuccessRows
			summary.ErrorRows += t.State.ErrorRows
			if len(t.State.ScanTaskErr) > 0 {
				allErr = multierr.Append(allErr, errors.New(t.State.ScanTaskErr))
			}
		}

		summary.TotalScanTask += 1
		if t.Status != cache.TaskStatusWaiting {
			summary.ScheduledScanTask += 1
		}
		if t.Status == cache.TaskStatusFinished {
			summary.FinishedScanTask += 1
		}
	}
	if allErr != nil {
		summary.ScanTaskErr = allErr.Error()
	}

	buf, err := json.Marshal(summary)
	if err != nil {
		return nil, err
	}
	summary.SummaryText = string(buf)
	return summary, nil
}

// DoGC deletes some old TTL job histories and redundant scan tasks
func (m *JobManager) DoGC(ctx context.Context, se session.Session, now time.Time) {
	if !m.isLeader() {
		// only the leader can do the GC to reduce the performance impact
		return
	}

	logutil.Logger(m.ctx).Info("TTL leader to DoGC")
	// Remove the table not exist in info schema cache.
	// Delete the table status before deleting the tasks. Therefore the related tasks
	if err := m.updateInfoSchemaCache(se); err == nil {
		// only remove table status after updating info schema without error
		existIDs := make([]int64, 0, len(m.infoSchemaCache.Tables))
		for id := range m.infoSchemaCache.Tables {
			existIDs = append(existIDs, id)
		}
		sql := gcTTLTableStatusGCSQL(existIDs)
		if _, err := se.ExecuteSQL(ctx, sql); err != nil {
			logutil.Logger(ctx).Warn("fail to gc ttl table status", zap.Error(err))
		}
	} else {
		logutil.Logger(m.ctx).Warn("failed to update info schema cache", zap.Error(err))
	}

	if _, err := se.ExecuteSQL(ctx, taskGCTemplate); err != nil {
		logutil.Logger(ctx).Warn("fail to gc redundant scan task", zap.Error(err))
	}

	if _, err := se.ExecuteSQL(ctx, ttlJobHistoryGCTemplate); err != nil {
		logutil.Logger(ctx).Warn("fail to gc ttl job history", zap.Error(err))
	}
}

// GetDelayMetricRecords gets the records of TTL delay metrics
func GetDelayMetricRecords(ctx context.Context, se session.Session, now time.Time) (map[int64]*metrics.DelayMetricsRecord, error) {
	sql := `SELECT
    parent_table_id as tid,
    CAST(UNIX_TIMESTAMP(MIN(create_time)) AS SIGNED) as job_ts
FROM
    (
        SELECT
            table_id,
            parent_table_id,
            MAX(create_time) AS create_time
        FROM
            mysql.tidb_ttl_job_history
        WHERE
            create_time > CURDATE() - INTERVAL 7 DAY
            AND status = 'finished'
            AND JSON_VALID(summary_text)
            AND summary_text ->> "$.scan_task_err" IS NULL
        GROUP BY
            table_id,
            parent_table_id
    ) t
GROUP BY
    parent_table_id;`

	rows, err := se.ExecuteSQL(ctx, sql)
	if err != nil {
		return nil, err
	}

	records := make(map[int64]*metrics.DelayMetricsRecord, len(rows))
	for _, row := range rows {
		r := &metrics.DelayMetricsRecord{
			TableID:     row.GetInt64(0),
			LastJobTime: time.Unix(row.GetInt64(1), 0),
		}

		if now.After(r.LastJobTime) {
			r.AbsoluteDelay = now.Sub(r.LastJobTime)
		}
		records[r.TableID] = r
	}

	isVer := se.GetLatestInfoSchema()
	is, ok := isVer.(infoschema.InfoSchema)
	if !ok {
		logutil.Logger(ctx).Error(fmt.Sprintf("failed to cast information schema for type: %v", isVer))
		return records, nil
	}

	noRecordTables := make([]string, 0)
	ch := is.ListTablesWithSpecialAttribute(infoschemacontext.TTLAttribute)
	for _, v := range ch {
		for _, tblInfo := range v.TableInfos {
			interval, err := tblInfo.TTLInfo.GetJobInterval()
			if err != nil {
				logutil.Logger(ctx).Error("failed to get table's job interval",
					zap.Error(err),
					zap.String("db", v.DBName.O),
					zap.String("table", tblInfo.Name.String()),
				)
				interval = time.Hour
			}

			record, ok := records[tblInfo.ID]
			if !ok {
				noRecordTables = append(noRecordTables, strconv.FormatInt(tblInfo.ID, 10))
				continue
			}

			if record.AbsoluteDelay > interval {
				record.ScheduleRelativeDelay = record.AbsoluteDelay - interval
			}
		}
	}

	if len(noRecordTables) > 0 {
		sql = fmt.Sprintf("select TIDB_TABLE_ID, CAST(UNIX_TIMESTAMP(CREATE_TIME) AS SIGNED) from information_schema.tables WHERE TIDB_TABLE_ID in (%s)", strings.Join(noRecordTables, ", "))
		if rows, err = se.ExecuteSQL(ctx, sql); err != nil {
			logutil.Logger(ctx).Error("failed to exec sql",
				zap.Error(err),
				zap.String("sql", sql),
			)
		} else {
			for _, row := range rows {
				tblID := row.GetInt64(0)
				tblCreateTime := time.Unix(row.GetInt64(1), 0)
				r := &metrics.DelayMetricsRecord{
					TableID: tblID,
				}

				if now.After(tblCreateTime) {
					r.AbsoluteDelay = now.Sub(tblCreateTime)
					r.ScheduleRelativeDelay = r.AbsoluteDelay
				}

				records[tblID] = r
			}
		}
	}

	return records, nil
}

// SubmitTTLManagerJobRequest is the request to submit a TTL job to manager
type SubmitTTLManagerJobRequest struct {
	// TableID indicates the parent table id
	TableID int64
	//  PhysicalID indicates the physical table id
	PhysicalID int64
	//  RequestID indicates the request id of the job
	RequestID string
	// RespCh indicates the channel for response
	RespCh chan<- error
}

type managerJobAdapter struct {
	store     kv.Storage
	sessPool  syssession.Pool
	requestCh chan<- *SubmitTTLManagerJobRequest
}

// NewManagerJobAdapter creates a managerJobAdapter
func NewManagerJobAdapter(store kv.Storage, sessPool syssession.Pool, requestCh chan<- *SubmitTTLManagerJobRequest) TTLJobAdapter {
	return &managerJobAdapter{store: store, sessPool: sessPool, requestCh: requestCh}
}

func (a *managerJobAdapter) CanSubmitJob(tableID, physicalID int64) (ok bool) {
	err := withSession(a.sessPool, func(se session.Session) (internalErr error) {
		ok, internalErr = a.canSubmitJobWithSession(tableID, physicalID, se)
		return
	})

	if err != nil {
		logutil.BgLogger().Error("fail to check whether can submit job", zap.Error(err))
		return false
	}

	return ok
}

func (a *managerJobAdapter) canSubmitJobWithSession(tableID, physicalID int64, se session.Session) (bool, error) {
	is := se.GetLatestInfoSchema().(infoschema.InfoSchema)
	tbl, ok := is.TableByID(context.Background(), tableID)
	if !ok {
		return false, nil
	}

	tblInfo := tbl.Meta()
	ttlInfo := tblInfo.TTLInfo
	if ttlInfo == nil || !ttlInfo.Enable {
		return false, nil
	}

	if physicalID != tableID {
		if par := tbl.GetPartitionedTable(); par == nil || par.GetPartition(physicalID) == nil {
			return false, nil
		}
	}

	ctx, cancel := context.WithTimeout(context.TODO(), 5*time.Minute)
	defer cancel()

	selectTasksCntSQL := "select LOW_PRIORITY COUNT(1) FROM mysql.tidb_ttl_task WHERE status IN ('waiting', 'running')"
	rs, err := se.ExecuteSQL(ctx, selectTasksCntSQL)
	if err == nil && len(rs) == 0 {
		logutil.BgLogger().Error(
			"selectTasksCntSQL returns no row",
			zap.Int64("physicalID", physicalID),
			zap.Int64("tableID", tableID),
			zap.String("SQL", selectTasksCntSQL),
		)
		return false, nil
	}

	if err != nil {
		logutil.BgLogger().Error(
			"error to query ttl task count",
			zap.Error(err),
			zap.Int64("physicalID", physicalID),
			zap.Int64("tableID", tableID),
			zap.String("SQL", selectTasksCntSQL),
		)
		return false, err
	}

	cnt := rs[0].GetInt64(0)
	tasksLimit := getMaxRunningTasksLimit(a.store)
	if cnt >= int64(tasksLimit) {
		logutil.BgLogger().Warn(
			"current TTL tasks count exceeds limit, delay create new job temporarily",
			zap.Int64("physicalID", physicalID),
			zap.Int64("tableID", tableID),
			zap.Int64("count", cnt),
			zap.Int("limit", tasksLimit),
		)
		return false, nil
	}

	return true, nil
}

func (a *managerJobAdapter) SubmitJob(ctx context.Context, tableID, physicalID int64, requestID string, _ time.Time) (*TTLJobTrace, error) {
	respCh := make(chan error, 1)
	req := &SubmitTTLManagerJobRequest{
		TableID:    tableID,
		PhysicalID: physicalID,
		RequestID:  requestID,
		RespCh:     respCh,
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case a.requestCh <- req:
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-respCh:
			if err != nil {
				return nil, err
			}

			return &TTLJobTrace{
				RequestID: requestID,
				Finished:  false,
			}, nil
		}
	}
}

func (a *managerJobAdapter) GetJob(ctx context.Context, tableID, physicalID int64, requestID string) (*TTLJobTrace, error) {
	var job *TTLJobTrace
	err := withSession(a.sessPool, func(se session.Session) (internalErr error) {
		job, internalErr = a.getJobWithSession(ctx, se, tableID, physicalID, requestID)
		return
	})

	if err != nil {
		logutil.BgLogger().Error("fail to get job", zap.Error(err))
		return nil, err
	}

	return job, nil
}

func (a *managerJobAdapter) getJobWithSession(ctx context.Context, se session.Session, tableID, physicalID int64, requestID string) (*TTLJobTrace, error) {
	rows, err := se.ExecuteSQL(
		ctx,
		"select summary_text, status from mysql.tidb_ttl_job_history where table_id=%? AND parent_table_id=%? AND job_id=%?",
		physicalID, tableID, requestID,
	)
	if err != nil {
		return nil, err
	}

	if len(rows) == 0 {
		return nil, nil
	}

	jobTrace := TTLJobTrace{
		RequestID: requestID,
	}

	row := rows[0]
	if !row.IsNull(0) {
		if summaryBytes := row.GetBytes(0); len(summaryBytes) > 0 {
			var ttlSummary TTLSummary
			if err = json.Unmarshal(summaryBytes, &ttlSummary); err != nil {
				return nil, err
			}
			jobTrace.Summary = &ttlSummary
		}
	}

	if !row.IsNull(1) {
		statusText := row.GetString(1)
		switch cache.JobStatus(statusText) {
		case cache.JobStatusFinished, cache.JobStatusTimeout, cache.JobStatusCancelled:
			jobTrace.Finished = true
		}
	}

	return &jobTrace, nil
}

func (a *managerJobAdapter) Now() (now time.Time, _ error) {
	err := withSession(a.sessPool, func(se session.Session) error {
		tz, err := se.GlobalTimeZone(context.TODO())
		if err != nil {
			return err
		}
		now = se.Now().In(tz)
		return nil
	})
	return now, err
}

func (m *JobManager) jobLogger(job *ttlJob) *zap.Logger {
	logger := logutil.Logger(m.ctx)

	logger = logger.With(zap.String("jobID", job.id))
	logger = logger.With(zap.Int64("tableID", job.tableID))
	if tbl, ok := m.infoSchemaCache.Tables[job.tableID]; ok {
		logger = logger.With(zap.String("tableName", tbl.FullName()))
	} else {
		logger = logger.With(zap.String("tableName", "unknown"))
	}

	return logger
}
