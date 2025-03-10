// Copyright 2019 PingCAP, Inc.
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

package bindinfo

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/metrics"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/format"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/terror"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/sessionctx/stmtctx"
	"github.com/pingcap/tidb/pkg/sessionctx/variable"
	"github.com/pingcap/tidb/pkg/types"
	driver "github.com/pingcap/tidb/pkg/types/parser_driver"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/hint"
	"github.com/pingcap/tidb/pkg/util/logutil"
	utilparser "github.com/pingcap/tidb/pkg/util/parser"
	"github.com/pingcap/tidb/pkg/util/sqlexec"
	stmtsummaryv2 "github.com/pingcap/tidb/pkg/util/stmtsummary/v2"
	tablefilter "github.com/pingcap/tidb/pkg/util/table-filter"
	"github.com/pingcap/tidb/pkg/util/timeutil"
	"go.uber.org/zap"
	"golang.org/x/exp/maps"
)

// BindHandle is used to handle all global sql bind operations.
type BindHandle struct {
	sctx struct {
		sync.Mutex
		sessionctx.Context
	}

	// bindInfo caches the sql bind info from storage.
	//
	// The Mutex protects that there is only one goroutine changes the content
	// of atomic.Value.
	//
	// NOTE: Concurrent Value Write:
	//
	//    bindInfo.Lock()
	//    newCache := bindInfo.Value.Load()
	//    do the write operation on the newCache
	//    bindInfo.Value.Store(newCache)
	//    bindInfo.Unlock()
	//
	// NOTE: Concurrent Value Read:
	//
	//    cache := bindInfo.Load().
	//    read the content
	//
	bindInfo struct {
		sync.Mutex
		atomic.Value
		parser         *parser.Parser
		lastUpdateTime types.Time
	}

	// invalidBindRecordMap indicates the invalid bind records found during querying.
	// A record will be deleted from this map, after 2 bind-lease, after it is dropped from the kv.
	invalidBindRecordMap tmpBindRecordMap

	// pendingVerifyBindRecordMap indicates the pending verify bind records that found during query.
	pendingVerifyBindRecordMap tmpBindRecordMap
}

// Lease influences the duration of loading bind info and handling invalid bind.
var Lease = 3 * time.Second

const (
	// OwnerKey is the bindinfo owner path that is saved to etcd.
	OwnerKey = "/tidb/bindinfo/owner"
	// Prompt is the prompt for bindinfo owner manager.
	Prompt = "bindinfo"
	// BuiltinPseudoSQL4BindLock is used to simulate LOCK TABLE for mysql.bind_info.
	BuiltinPseudoSQL4BindLock = "builtin_pseudo_sql_for_bind_lock"
)

type bindRecordUpdate struct {
	bindRecord *BindRecord
	updateTime time.Time
}

// NewBindHandle creates a new BindHandle.
func NewBindHandle(ctx sessionctx.Context) *BindHandle {
	handle := &BindHandle{}
	handle.Reset(ctx)
	return handle
}

// Reset is to reset the BindHandle and clean old info.
func (h *BindHandle) Reset(ctx sessionctx.Context) {
	h.bindInfo.Lock()
	defer h.bindInfo.Unlock()
	h.sctx.Context = ctx
	h.bindInfo.Value.Store(newBindCache())
	h.bindInfo.parser = parser.New()
	h.invalidBindRecordMap.Value.Store(make(map[string]*bindRecordUpdate))
	h.invalidBindRecordMap.flushFunc = func(record *BindRecord) error {
		_, err := h.DropBindRecord(record.OriginalSQL, record.Db, &record.Bindings[0])
		return err
	}
	h.pendingVerifyBindRecordMap.Value.Store(make(map[string]*bindRecordUpdate))
	h.pendingVerifyBindRecordMap.flushFunc = func(record *BindRecord) error {
		// BindSQL has already been validated when coming here, so we use nil sctx parameter.
		return h.AddBindRecord(nil, record)
	}
	variable.RegisterStatistics(h)
}

// Update updates the global sql bind cache.
func (h *BindHandle) Update(fullLoad bool) (err error) {
	h.bindInfo.Lock()
	lastUpdateTime := h.bindInfo.lastUpdateTime
	var timeCondition string
	if !fullLoad {
		timeCondition = fmt.Sprintf("WHERE update_time>'%s'", lastUpdateTime.String())
	}

	exec := h.sctx.Context.(sqlexec.RestrictedSQLExecutor)

	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnBindInfo)
	// No need to acquire the session context lock for ExecRestrictedSQL, it
	// uses another background session.
	selectStmt := fmt.Sprintf(`SELECT original_sql, bind_sql, default_db, status, create_time,
       update_time, charset, collation, source, sql_digest, plan_digest FROM mysql.bind_info
       %s ORDER BY update_time, create_time`, timeCondition)
	rows, _, err := exec.ExecRestrictedSQL(ctx, nil, selectStmt)

	if err != nil {
		h.bindInfo.Unlock()
		return err
	}

	newCache, memExceededErr := h.bindInfo.Value.Load().(*bindCache).Copy()
	defer func() {
		h.bindInfo.lastUpdateTime = lastUpdateTime
		h.bindInfo.Value.Store(newCache)
		h.bindInfo.Unlock()
	}()

	for _, row := range rows {
		// If the memory usage of the binding_cache exceeds its capacity, we will break and do not handle.
		if memExceededErr != nil {
			break
		}
		// Skip the builtin record which is designed for binding synchronization.
		if row.GetString(0) == BuiltinPseudoSQL4BindLock {
			continue
		}
		hash, meta, err := h.newBindRecord(row)

		// Update lastUpdateTime to the newest one.
		// Even if this one is an invalid bind.
		if meta.Bindings[0].UpdateTime.Compare(lastUpdateTime) > 0 {
			lastUpdateTime = meta.Bindings[0].UpdateTime
		}

		if err != nil {
			logutil.BgLogger().Debug("failed to generate bind record from data row", zap.String("category", "sql-bind"), zap.Error(err))
			continue
		}

		oldRecord := newCache.GetBindRecord(hash, meta.OriginalSQL, meta.Db)
		newRecord := merge(oldRecord, meta).removeDeletedBindings()
		if len(newRecord.Bindings) > 0 {
			err = newCache.SetBindRecord(hash, newRecord)
			if err != nil {
				memExceededErr = err
			}
		} else {
			newCache.RemoveBindRecord(hash, newRecord)
		}
		updateMetrics(metrics.ScopeGlobal, oldRecord, newCache.GetBindRecord(hash, meta.OriginalSQL, meta.Db), true)
	}
	if memExceededErr != nil {
		// When the memory capacity of bing_cache is not enough,
		// there will be some memory-related errors in multiple places.
		// Only needs to be handled once.
		logutil.BgLogger().Warn("BindHandle.Update", zap.String("category", "sql-bind"), zap.Error(memExceededErr))
	}
	return nil
}

// CreateBindRecord creates a BindRecord to the storage and the cache.
// It replaces all the exists bindings for the same normalized SQL.
func (h *BindHandle) CreateBindRecord(sctx sessionctx.Context, record *BindRecord) (err error) {
	err = record.prepareHints(sctx)
	if err != nil {
		return err
	}

	record.Db = strings.ToLower(record.Db)
	h.bindInfo.Lock()
	h.sctx.Lock()
	defer func() {
		h.sctx.Unlock()
		h.bindInfo.Unlock()
	}()
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnBindInfo)
	exec, _ := h.sctx.Context.(sqlexec.SQLExecutor)
	_, err = exec.ExecuteInternal(ctx, "BEGIN PESSIMISTIC")
	if err != nil {
		return
	}

	defer func() {
		if err != nil {
			_, err1 := exec.ExecuteInternal(ctx, "ROLLBACK")
			terror.Log(err1)
			return
		}

		_, err = exec.ExecuteInternal(ctx, "COMMIT")
		if err != nil {
			return
		}

		sqlDigest := parser.DigestNormalized(record.OriginalSQL)
		h.setBindRecord(sqlDigest.String(), record)
	}()

	// Lock mysql.bind_info to synchronize with CreateBindRecord / AddBindRecord / DropBindRecord on other tidb instances.
	if err = h.lockBindInfoTable(); err != nil {
		return err
	}

	now := types.NewTime(types.FromGoTime(time.Now()), mysql.TypeTimestamp, 3)

	updateTs := now.String()
	_, err = exec.ExecuteInternal(ctx, `UPDATE mysql.bind_info SET status = %?, update_time = %? WHERE original_sql = %? AND update_time < %?`,
		deleted, updateTs, record.OriginalSQL, updateTs)
	if err != nil {
		return err
	}

	for i := range record.Bindings {
		record.Bindings[i].CreateTime = now
		record.Bindings[i].UpdateTime = now

		// Insert the BindRecord to the storage.
		_, err = exec.ExecuteInternal(ctx, `INSERT INTO mysql.bind_info VALUES (%?,%?, %?, %?, %?, %?, %?, %?, %?, %?, %?)`,
			record.OriginalSQL,
			record.Bindings[i].BindSQL,
			record.Db,
			record.Bindings[i].Status,
			record.Bindings[i].CreateTime.String(),
			record.Bindings[i].UpdateTime.String(),
			record.Bindings[i].Charset,
			record.Bindings[i].Collation,
			record.Bindings[i].Source,
			record.Bindings[i].SQLDigest,
			record.Bindings[i].PlanDigest,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// AddBindRecord adds a BindRecord to the storage and BindRecord to the cache.
func (h *BindHandle) AddBindRecord(sctx sessionctx.Context, record *BindRecord) (err error) {
	err = record.prepareHints(sctx)
	if err != nil {
		return err
	}

	record.Db = strings.ToLower(record.Db)
	oldRecord := h.GetBindRecord(parser.DigestNormalized(record.OriginalSQL).String(), record.OriginalSQL, record.Db)
	var duplicateBinding *Binding
	if oldRecord != nil {
		binding := oldRecord.FindBinding(record.Bindings[0].ID)
		if binding != nil {
			// There is already a binding with status `Enabled`, `Disabled`, `PendingVerify` or `Rejected`, we could directly cancel the job.
			if record.Bindings[0].Status == PendingVerify {
				return nil
			}
			// Otherwise, we need to remove it before insert.
			duplicateBinding = binding
		}
	}

	h.bindInfo.Lock()
	h.sctx.Lock()
	defer func() {
		h.sctx.Unlock()
		h.bindInfo.Unlock()
	}()
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnBindInfo)
	exec, _ := h.sctx.Context.(sqlexec.SQLExecutor)
	_, err = exec.ExecuteInternal(ctx, "BEGIN PESSIMISTIC")
	if err != nil {
		return
	}

	defer func() {
		if err != nil {
			_, err1 := exec.ExecuteInternal(ctx, "ROLLBACK")
			terror.Log(err1)
			return
		}

		_, err = exec.ExecuteInternal(ctx, "COMMIT")
		if err != nil {
			return
		}

		h.appendBindRecord(parser.DigestNormalized(record.OriginalSQL).String(), record)
	}()

	// Lock mysql.bind_info to synchronize with CreateBindRecord / AddBindRecord / DropBindRecord on other tidb instances.
	if err = h.lockBindInfoTable(); err != nil {
		return err
	}
	if duplicateBinding != nil {
		_, err = exec.ExecuteInternal(ctx, `DELETE FROM mysql.bind_info WHERE original_sql = %? AND bind_sql = %?`, record.OriginalSQL, duplicateBinding.BindSQL)
		if err != nil {
			return err
		}
	}

	now := types.NewTime(types.FromGoTime(time.Now()), mysql.TypeTimestamp, 3)
	for i := range record.Bindings {
		if duplicateBinding != nil {
			record.Bindings[i].CreateTime = duplicateBinding.CreateTime
		} else {
			record.Bindings[i].CreateTime = now
		}
		record.Bindings[i].UpdateTime = now

		if record.Bindings[i].SQLDigest == "" {
			parser4binding := parser.New()
			var originNode ast.StmtNode
			originNode, err = parser4binding.ParseOneStmt(record.OriginalSQL, record.Bindings[i].Charset, record.Bindings[i].Collation)
			if err != nil {
				return err
			}
			_, sqlDigestWithDB := parser.NormalizeDigest(utilparser.RestoreWithDefaultDB(originNode, record.Db, record.OriginalSQL))
			record.Bindings[i].SQLDigest = sqlDigestWithDB.String()
		}
		// Insert the BindRecord to the storage.
		_, err = exec.ExecuteInternal(ctx, `INSERT INTO mysql.bind_info VALUES (%?, %?, %?, %?, %?, %?, %?, %?, %?, %?, %?)`,
			record.OriginalSQL,
			record.Bindings[i].BindSQL,
			record.Db,
			record.Bindings[i].Status,
			record.Bindings[i].CreateTime.String(),
			record.Bindings[i].UpdateTime.String(),
			record.Bindings[i].Charset,
			record.Bindings[i].Collation,
			record.Bindings[i].Source,
			record.Bindings[i].SQLDigest,
			record.Bindings[i].PlanDigest,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// DropBindRecord drops a BindRecord to the storage and BindRecord int the cache.
func (h *BindHandle) DropBindRecord(originalSQL, db string, binding *Binding) (deletedRows uint64, err error) {
	db = strings.ToLower(db)
	h.bindInfo.Lock()
	h.sctx.Lock()
	defer func() {
		h.sctx.Unlock()
		h.bindInfo.Unlock()
	}()
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnBindInfo)
	exec, _ := h.sctx.Context.(sqlexec.SQLExecutor)
	_, err = exec.ExecuteInternal(ctx, "BEGIN PESSIMISTIC")
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_, err1 := exec.ExecuteInternal(ctx, "ROLLBACK")
			terror.Log(err1)
			return
		}

		_, err = exec.ExecuteInternal(ctx, "COMMIT")
		if err != nil || deletedRows == 0 {
			return
		}

		record := &BindRecord{OriginalSQL: originalSQL, Db: db}
		if binding != nil {
			record.Bindings = append(record.Bindings, *binding)
		}
		h.removeBindRecord(parser.DigestNormalized(originalSQL).String(), record)
	}()

	// Lock mysql.bind_info to synchronize with CreateBindRecord / AddBindRecord / DropBindRecord on other tidb instances.
	if err = h.lockBindInfoTable(); err != nil {
		return 0, err
	}

	updateTs := types.NewTime(types.FromGoTime(time.Now()), mysql.TypeTimestamp, 3).String()

	if binding == nil {
		_, err = exec.ExecuteInternal(ctx, `UPDATE mysql.bind_info SET status = %?, update_time = %? WHERE original_sql = %? AND update_time < %? AND status != %?`,
			deleted, updateTs, originalSQL, updateTs, deleted)
	} else {
		_, err = exec.ExecuteInternal(ctx, `UPDATE mysql.bind_info SET status = %?, update_time = %? WHERE original_sql = %? AND update_time < %? AND bind_sql = %? and status != %?`,
			deleted, updateTs, originalSQL, updateTs, binding.BindSQL, deleted)
	}
	if err != nil {
		return 0, err
	}

	return h.sctx.Context.GetSessionVars().StmtCtx.AffectedRows(), nil
}

// DropBindRecordByDigest drop BindRecord to the storage and BindRecord int the cache.
func (h *BindHandle) DropBindRecordByDigest(sqlDigest string) (deletedRows uint64, err error) {
	oldRecord, err := h.GetBindRecordBySQLDigest(sqlDigest)
	if err != nil {
		return 0, err
	}
	return h.DropBindRecord(oldRecord.OriginalSQL, strings.ToLower(oldRecord.Db), nil)
}

// SetBindRecordStatus set a BindRecord's status to the storage and bind cache.
func (h *BindHandle) SetBindRecordStatus(originalSQL string, binding *Binding, newStatus string) (ok bool, err error) {
	h.bindInfo.Lock()
	h.sctx.Lock()
	defer func() {
		h.sctx.Unlock()
		h.bindInfo.Unlock()
	}()
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnBindInfo)
	exec, _ := h.sctx.Context.(sqlexec.SQLExecutor)
	_, err = exec.ExecuteInternal(ctx, "BEGIN PESSIMISTIC")
	if err != nil {
		return
	}
	var (
		updateTs               types.Time
		oldStatus0, oldStatus1 string
		affectRows             int
	)
	if newStatus == Disabled {
		// For compatibility reasons, when we need to 'set binding disabled for <stmt>',
		// we need to consider both the 'enabled' and 'using' status.
		oldStatus0 = Using
		oldStatus1 = Enabled
	} else if newStatus == Enabled {
		// In order to unify the code, two identical old statuses are set.
		oldStatus0 = Disabled
		oldStatus1 = Disabled
	}
	defer func() {
		if err != nil {
			_, err1 := exec.ExecuteInternal(ctx, "ROLLBACK")
			terror.Log(err1)
			return
		}

		_, err = exec.ExecuteInternal(ctx, "COMMIT")
		if err != nil {
			return
		}
		if affectRows == 0 {
			return
		}

		// The set binding status operation is success.
		ok = true
		record := &BindRecord{OriginalSQL: originalSQL}
		sqlDigest := parser.DigestNormalized(record.OriginalSQL)
		oldRecord := h.GetBindRecord(sqlDigest.String(), originalSQL, "")
		setBindingStatusInCacheSucc := false
		if oldRecord != nil && len(oldRecord.Bindings) > 0 {
			record.Bindings = make([]Binding, len(oldRecord.Bindings))
			copy(record.Bindings, oldRecord.Bindings)
			for ind, oldBinding := range record.Bindings {
				if oldBinding.Status == oldStatus0 || oldBinding.Status == oldStatus1 {
					if binding == nil || (binding != nil && oldBinding.isSame(binding)) {
						setBindingStatusInCacheSucc = true
						record.Bindings[ind].Status = newStatus
						record.Bindings[ind].UpdateTime = updateTs
					}
				}
			}
		}
		if setBindingStatusInCacheSucc {
			h.setBindRecord(sqlDigest.String(), record)
		}
	}()

	// Lock mysql.bind_info to synchronize with SetBindingStatus on other tidb instances.
	if err = h.lockBindInfoTable(); err != nil {
		return
	}

	updateTs = types.NewTime(types.FromGoTime(time.Now()), mysql.TypeTimestamp, 3)
	updateTsStr := updateTs.String()

	if binding == nil {
		_, err = exec.ExecuteInternal(ctx, `UPDATE mysql.bind_info SET status = %?, update_time = %? WHERE original_sql = %? AND update_time < %? AND status IN (%?, %?)`,
			newStatus, updateTsStr, originalSQL, updateTsStr, oldStatus0, oldStatus1)
	} else {
		_, err = exec.ExecuteInternal(ctx, `UPDATE mysql.bind_info SET status = %?, update_time = %? WHERE original_sql = %? AND update_time < %? AND bind_sql = %? AND status IN (%?, %?)`,
			newStatus, updateTsStr, originalSQL, updateTsStr, binding.BindSQL, oldStatus0, oldStatus1)
	}
	affectRows = int(h.sctx.Context.GetSessionVars().StmtCtx.AffectedRows())
	return
}

// SetBindRecordStatusByDigest set a BindRecord's status to the storage and bind cache.
func (h *BindHandle) SetBindRecordStatusByDigest(newStatus, sqlDigest string) (ok bool, err error) {
	oldRecord, err := h.GetBindRecordBySQLDigest(sqlDigest)
	if err != nil {
		return false, err
	}
	return h.SetBindRecordStatus(oldRecord.OriginalSQL, nil, newStatus)
}

// GCBindRecord physically removes the deleted bind records in mysql.bind_info.
func (h *BindHandle) GCBindRecord() (err error) {
	h.bindInfo.Lock()
	h.sctx.Lock()
	defer func() {
		h.sctx.Unlock()
		h.bindInfo.Unlock()
	}()
	exec, _ := h.sctx.Context.(sqlexec.SQLExecutor)
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnBindInfo)
	_, err = exec.ExecuteInternal(ctx, "BEGIN PESSIMISTIC")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_, err1 := exec.ExecuteInternal(ctx, "ROLLBACK")
			terror.Log(err1)
			return
		}

		_, err = exec.ExecuteInternal(ctx, "COMMIT")
		if err != nil {
			return
		}
	}()

	// Lock mysql.bind_info to synchronize with CreateBindRecord / AddBindRecord / DropBindRecord on other tidb instances.
	if err = h.lockBindInfoTable(); err != nil {
		return err
	}

	// To make sure that all the deleted bind records have been acknowledged to all tidb,
	// we only garbage collect those records with update_time before 10 leases.
	updateTime := time.Now().Add(-(10 * Lease))
	updateTimeStr := types.NewTime(types.FromGoTime(updateTime), mysql.TypeTimestamp, 3).String()
	_, err = exec.ExecuteInternal(ctx, `DELETE FROM mysql.bind_info WHERE status = 'deleted' and update_time < %?`, updateTimeStr)
	return err
}

// lockBindInfoTable simulates `LOCK TABLE mysql.bind_info WRITE` by acquiring a pessimistic lock on a
// special builtin row of mysql.bind_info. Note that this function must be called with h.sctx.Lock() held.
// We can replace this implementation to normal `LOCK TABLE mysql.bind_info WRITE` if that feature is
// generally available later.
// This lock would enforce the CREATE / DROP GLOBAL BINDING statements to be executed sequentially,
// even if they come from different tidb instances.
func (h *BindHandle) lockBindInfoTable() error {
	// h.sctx already locked.
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnBindInfo)
	exec, _ := h.sctx.Context.(sqlexec.SQLExecutor)
	_, err := exec.ExecuteInternal(ctx, h.LockBindInfoSQL())
	return err
}

// LockBindInfoSQL simulates LOCK TABLE by updating a same row in each pessimistic transaction.
func (*BindHandle) LockBindInfoSQL() string {
	sql, err := sqlexec.EscapeSQL("UPDATE mysql.bind_info SET source= %? WHERE original_sql= %?", Builtin, BuiltinPseudoSQL4BindLock)
	if err != nil {
		return ""
	}
	return sql
}

// tmpBindRecordMap is used to temporarily save bind record changes.
// Those changes will be flushed into store periodically.
type tmpBindRecordMap struct {
	sync.Mutex
	atomic.Value
	flushFunc func(record *BindRecord) error
}

// flushToStore calls flushFunc for items in tmpBindRecordMap and removes them with a delay.
func (tmpMap *tmpBindRecordMap) flushToStore() {
	tmpMap.Lock()
	defer tmpMap.Unlock()
	newMap := copyBindRecordUpdateMap(tmpMap.Load().(map[string]*bindRecordUpdate))
	for key, bindRecord := range newMap {
		if bindRecord.updateTime.IsZero() {
			err := tmpMap.flushFunc(bindRecord.bindRecord)
			if err != nil {
				logutil.BgLogger().Debug("flush bind record failed", zap.String("category", "sql-bind"), zap.Error(err))
			}
			bindRecord.updateTime = time.Now()
			continue
		}

		if time.Since(bindRecord.updateTime) > 6*time.Second {
			delete(newMap, key)
			updateMetrics(metrics.ScopeGlobal, bindRecord.bindRecord, nil, false)
		}
	}
	tmpMap.Store(newMap)
}

// Add puts a BindRecord into tmpBindRecordMap.
func (tmpMap *tmpBindRecordMap) Add(bindRecord *BindRecord) {
	key := bindRecord.OriginalSQL + ":" + bindRecord.Db + ":" + bindRecord.Bindings[0].ID
	if _, ok := tmpMap.Load().(map[string]*bindRecordUpdate)[key]; ok {
		return
	}
	tmpMap.Lock()
	defer tmpMap.Unlock()
	if _, ok := tmpMap.Load().(map[string]*bindRecordUpdate)[key]; ok {
		return
	}
	newMap := copyBindRecordUpdateMap(tmpMap.Load().(map[string]*bindRecordUpdate))
	newMap[key] = &bindRecordUpdate{
		bindRecord: bindRecord,
	}
	tmpMap.Store(newMap)
	updateMetrics(metrics.ScopeGlobal, nil, bindRecord, false)
}

// DropInvalidBindRecord executes the drop BindRecord tasks.
func (h *BindHandle) DropInvalidBindRecord() {
	h.invalidBindRecordMap.flushToStore()
}

// AddDropInvalidBindTask adds BindRecord which needs to be deleted into invalidBindRecordMap.
func (h *BindHandle) AddDropInvalidBindTask(invalidBindRecord *BindRecord) {
	h.invalidBindRecordMap.Add(invalidBindRecord)
}

// Size returns the size of bind info cache.
func (h *BindHandle) Size() int {
	size := len(h.bindInfo.Load().(*bindCache).GetAllBindRecords())
	return size
}

// GetBindRecord returns the BindRecord of the (normdOrigSQL,db) if BindRecord exist.
func (h *BindHandle) GetBindRecord(hash, normdOrigSQL, db string) *BindRecord {
	return h.bindInfo.Load().(*bindCache).GetBindRecord(hash, normdOrigSQL, db)
}

// GetBindRecordBySQLDigest returns the BindRecord of the sql digest.
func (h *BindHandle) GetBindRecordBySQLDigest(sqlDigest string) (*BindRecord, error) {
	return h.bindInfo.Load().(*bindCache).GetBindRecordBySQLDigest(sqlDigest)
}

// GetAllBindRecord returns all bind records in cache.
func (h *BindHandle) GetAllBindRecord() (bindRecords []*BindRecord) {
	return h.bindInfo.Load().(*bindCache).GetAllBindRecords()
}

// SetBindCacheCapacity reset the capacity for the bindCache.
// It will not affect already cached BindRecords.
func (h *BindHandle) SetBindCacheCapacity(capacity int64) {
	h.bindInfo.Load().(*bindCache).SetMemCapacity(capacity)
}

// GetMemUsage returns the memory usage for the bind cache.
func (h *BindHandle) GetMemUsage() (memUsage int64) {
	return h.bindInfo.Load().(*bindCache).GetMemUsage()
}

// GetMemCapacity returns the memory capacity for the bind cache.
func (h *BindHandle) GetMemCapacity() (memCapacity int64) {
	return h.bindInfo.Load().(*bindCache).GetMemCapacity()
}

// newBindRecord builds BindRecord from a tuple in storage.
func (h *BindHandle) newBindRecord(row chunk.Row) (string, *BindRecord, error) {
	status := row.GetString(3)
	// For compatibility, the 'Using' status binding will be converted to the 'Enabled' status binding.
	if status == Using {
		status = Enabled
	}
	hint := Binding{
		BindSQL:    row.GetString(1),
		Status:     status,
		CreateTime: row.GetTime(4),
		UpdateTime: row.GetTime(5),
		Charset:    row.GetString(6),
		Collation:  row.GetString(7),
		Source:     row.GetString(8),
		SQLDigest:  row.GetString(9),
		PlanDigest: row.GetString(10),
	}
	bindRecord := &BindRecord{
		OriginalSQL: row.GetString(0),
		Db:          strings.ToLower(row.GetString(2)),
		Bindings:    []Binding{hint},
	}
	hash := parser.DigestNormalized(bindRecord.OriginalSQL)
	h.sctx.Lock()
	defer h.sctx.Unlock()
	h.sctx.GetSessionVars().CurrentDB = bindRecord.Db
	err := bindRecord.prepareHints(h.sctx.Context)
	return hash.String(), bindRecord, err
}

// setBindRecord sets the BindRecord to the cache, if there already exists a BindRecord,
// it will be overridden.
func (h *BindHandle) setBindRecord(hash string, meta *BindRecord) {
	newCache, err0 := h.bindInfo.Value.Load().(*bindCache).Copy()
	if err0 != nil {
		logutil.BgLogger().Warn("BindHandle.setBindRecord", zap.String("category", "sql-bind"), zap.Error(err0))
	}
	oldRecord := newCache.GetBindRecord(hash, meta.OriginalSQL, meta.Db)
	err1 := newCache.SetBindRecord(hash, meta)
	if err1 != nil && err0 == nil {
		logutil.BgLogger().Warn("BindHandle.setBindRecord", zap.String("category", "sql-bind"), zap.Error(err1))
	}
	h.bindInfo.Value.Store(newCache)
	updateMetrics(metrics.ScopeGlobal, oldRecord, meta, false)
}

// appendBindRecord adds the BindRecord to the cache, all the stale BindRecords are
// removed from the cache after this operation.
func (h *BindHandle) appendBindRecord(hash string, meta *BindRecord) {
	newCache, err0 := h.bindInfo.Value.Load().(*bindCache).Copy()
	if err0 != nil {
		logutil.BgLogger().Warn("BindHandle.appendBindRecord", zap.String("category", "sql-bind"), zap.Error(err0))
	}
	oldRecord := newCache.GetBindRecord(hash, meta.OriginalSQL, meta.Db)
	newRecord := merge(oldRecord, meta)
	err1 := newCache.SetBindRecord(hash, newRecord)
	if err1 != nil && err0 == nil {
		// Only need to handle the error once.
		logutil.BgLogger().Warn("BindHandle.appendBindRecord", zap.String("category", "sql-bind"), zap.Error(err1))
	}
	h.bindInfo.Value.Store(newCache)
	updateMetrics(metrics.ScopeGlobal, oldRecord, newRecord, false)
}

// removeBindRecord removes the BindRecord from the cache.
func (h *BindHandle) removeBindRecord(hash string, meta *BindRecord) {
	newCache, err := h.bindInfo.Value.Load().(*bindCache).Copy()
	if err != nil {
		logutil.BgLogger().Warn("", zap.String("category", "sql-bind"), zap.Error(err))
	}
	oldRecord := newCache.GetBindRecord(hash, meta.OriginalSQL, meta.Db)
	newCache.RemoveBindRecord(hash, meta)
	h.bindInfo.Value.Store(newCache)
	updateMetrics(metrics.ScopeGlobal, oldRecord, newCache.GetBindRecord(hash, meta.OriginalSQL, meta.Db), false)
}

func copyBindRecordUpdateMap(oldMap map[string]*bindRecordUpdate) map[string]*bindRecordUpdate {
	newMap := make(map[string]*bindRecordUpdate, len(oldMap))
	maps.Copy(newMap, oldMap)
	return newMap
}

type captureFilter struct {
	frequency int64
	tables    []tablefilter.Filter // `schema.table`
	users     map[string]struct{}

	fail      bool
	currentDB string
}

func (cf *captureFilter) Enter(in ast.Node) (out ast.Node, skipChildren bool) {
	if x, ok := in.(*ast.TableName); ok {
		tblEntry := stmtctx.TableEntry{
			DB:    x.Schema.L,
			Table: x.Name.L,
		}
		if x.Schema.L == "" {
			tblEntry.DB = cf.currentDB
		}
		for _, tableFilter := range cf.tables {
			if tableFilter.MatchTable(tblEntry.DB, tblEntry.Table) {
				cf.fail = true // some filter is matched
			}
		}
	}
	return in, cf.fail
}

func (*captureFilter) Leave(in ast.Node) (out ast.Node, ok bool) {
	return in, true
}

func (cf *captureFilter) isEmpty() bool {
	return len(cf.tables) == 0 && len(cf.users) == 0
}

// ParseCaptureTableFilter checks whether this filter is valid and parses it.
func ParseCaptureTableFilter(tableFilter string) (f tablefilter.Filter, valid bool) {
	// forbid wildcards '!' and '@' for safety,
	// please see https://github.com/pingcap/tidb-tools/tree/master/pkg/table-filter for more details.
	tableFilter = strings.TrimLeft(tableFilter, " \t")
	if tableFilter == "" {
		return nil, false
	}
	if tableFilter[0] == '!' || tableFilter[0] == '@' {
		return nil, false
	}
	var err error
	f, err = tablefilter.Parse([]string{tableFilter})
	if err != nil {
		return nil, false
	}
	return f, true
}

func (h *BindHandle) extractCaptureFilterFromStorage() (filter *captureFilter) {
	filter = &captureFilter{
		frequency: 1,
		users:     make(map[string]struct{}),
	}
	exec := h.sctx.Context.(sqlexec.RestrictedSQLExecutor)
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnBindInfo)
	// No need to acquire the session context lock for ExecRestrictedSQL, it
	// uses another background session.
	rows, _, err := exec.ExecRestrictedSQL(ctx, nil, `SELECT filter_type, filter_value FROM mysql.capture_plan_baselines_blacklist order by filter_type`)
	if err != nil {
		logutil.BgLogger().Warn("failed to load mysql.capture_plan_baselines_blacklist", zap.String("category", "sql-bind"), zap.Error(err))
		return
	}
	for _, row := range rows {
		filterTp := strings.ToLower(row.GetString(0))
		valStr := strings.ToLower(row.GetString(1))
		switch filterTp {
		case "table":
			tfilter, valid := ParseCaptureTableFilter(valStr)
			if !valid {
				logutil.BgLogger().Warn("capture table filter is invalid, ignore it", zap.String("category", "sql-bind"), zap.String("filter_value", valStr))
				continue
			}
			filter.tables = append(filter.tables, tfilter)
		case "user":
			filter.users[valStr] = struct{}{}
		case "frequency":
			f, err := strconv.ParseInt(valStr, 10, 64)
			if err != nil {
				logutil.BgLogger().Warn("failed to parse frequency type value, ignore it", zap.String("category", "sql-bind"), zap.String("filter_value", valStr), zap.Error(err))
				continue
			}
			if f < 1 {
				logutil.BgLogger().Warn("frequency threshold is less than 1, ignore it", zap.String("category", "sql-bind"), zap.Int64("frequency", f))
				continue
			}
			if f > filter.frequency {
				filter.frequency = f
			}
		default:
			logutil.BgLogger().Warn("unknown capture filter type, ignore it", zap.String("category", "sql-bind"), zap.String("filter_type", filterTp))
		}
	}
	return
}

// CaptureBaselines is used to automatically capture plan baselines.
func (h *BindHandle) CaptureBaselines() {
	parser4Capture := parser.New()
	captureFilter := h.extractCaptureFilterFromStorage()
	emptyCaptureFilter := captureFilter.isEmpty()
	bindableStmts := stmtsummaryv2.GetMoreThanCntBindableStmt(captureFilter.frequency)
	for _, bindableStmt := range bindableStmts {
		stmt, err := parser4Capture.ParseOneStmt(bindableStmt.Query, bindableStmt.Charset, bindableStmt.Collation)
		if err != nil {
			logutil.BgLogger().Debug("parse SQL failed in baseline capture", zap.String("category", "sql-bind"), zap.String("SQL", bindableStmt.Query), zap.Error(err))
			continue
		}
		if insertStmt, ok := stmt.(*ast.InsertStmt); ok && insertStmt.Select == nil {
			continue
		}
		if !emptyCaptureFilter {
			captureFilter.fail = false
			captureFilter.currentDB = bindableStmt.Schema
			stmt.Accept(captureFilter)
			if captureFilter.fail {
				continue
			}

			if len(captureFilter.users) > 0 {
				filteredByUser := true
				for user := range bindableStmt.Users {
					if _, ok := captureFilter.users[user]; !ok {
						filteredByUser = false // some user not in the black-list has processed this stmt
						break
					}
				}
				if filteredByUser {
					continue
				}
			}
		}
		dbName := utilparser.GetDefaultDB(stmt, bindableStmt.Schema)
		normalizedSQL, digest := parser.NormalizeDigest(utilparser.RestoreWithDefaultDB(stmt, dbName, bindableStmt.Query))
		if r := h.GetBindRecord(digest.String(), normalizedSQL, dbName); r != nil && r.HasAvailableBinding() {
			continue
		}
		bindSQL := GenerateBindSQL(context.TODO(), stmt, bindableStmt.PlanHint, true, dbName)
		if bindSQL == "" {
			continue
		}
		h.sctx.Lock()
		charset, collation := h.sctx.GetSessionVars().GetCharsetInfo()
		h.sctx.Unlock()
		binding := Binding{
			BindSQL:   bindSQL,
			Status:    Enabled,
			Charset:   charset,
			Collation: collation,
			Source:    Capture,
			SQLDigest: digest.String(),
		}
		// We don't need to pass the `sctx` because the BindSQL has been validated already.
		err = h.CreateBindRecord(nil, &BindRecord{OriginalSQL: normalizedSQL, Db: dbName, Bindings: []Binding{binding}})
		if err != nil {
			logutil.BgLogger().Debug("create bind record failed in baseline capture", zap.String("category", "sql-bind"), zap.String("SQL", bindableStmt.Query), zap.Error(err))
		}
	}
}

func getHintsForSQL(sctx sessionctx.Context, sql string) (string, error) {
	origVals := sctx.GetSessionVars().UsePlanBaselines
	sctx.GetSessionVars().UsePlanBaselines = false

	// Usually passing a sprintf to ExecuteInternal is not recommended, but in this case
	// it is safe because ExecuteInternal does not permit MultiStatement execution. Thus,
	// the statement won't be able to "break out" from EXPLAIN.
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnBindInfo)
	rs, err := sctx.(sqlexec.SQLExecutor).ExecuteInternal(ctx, fmt.Sprintf("EXPLAIN FORMAT='hint' %s", sql))
	sctx.GetSessionVars().UsePlanBaselines = origVals
	if rs != nil {
		defer func() {
			// Audit log is collected in Close(), set InRestrictedSQL to avoid 'create sql binding' been recorded as 'explain'.
			origin := sctx.GetSessionVars().InRestrictedSQL
			sctx.GetSessionVars().InRestrictedSQL = true
			terror.Call(rs.Close)
			sctx.GetSessionVars().InRestrictedSQL = origin
		}()
	}
	if err != nil {
		return "", err
	}
	chk := rs.NewChunk(nil)
	err = rs.Next(context.TODO(), chk)
	if err != nil {
		return "", err
	}
	return chk.GetRow(0).GetString(0), nil
}

// GenerateBindSQL generates binding sqls from stmt node and plan hints.
func GenerateBindSQL(ctx context.Context, stmtNode ast.StmtNode, planHint string, skipCheckIfHasParam bool, defaultDB string) string {
	// If would be nil for very simple cases such as point get, we do not need to evolve for them.
	if planHint == "" {
		return ""
	}
	if !skipCheckIfHasParam {
		paramChecker := &paramMarkerChecker{}
		stmtNode.Accept(paramChecker)
		// We need to evolve on current sql, but we cannot restore values for paramMarkers yet,
		// so just ignore them now.
		if paramChecker.hasParamMarker {
			return ""
		}
	}
	// We need to evolve plan based on the current sql, not the original sql which may have different parameters.
	// So here we would remove the hint and inject the current best plan hint.
	hint.BindHint(stmtNode, &hint.HintsSet{})
	bindSQL := utilparser.RestoreWithDefaultDB(stmtNode, defaultDB, "")
	if bindSQL == "" {
		return ""
	}
	switch n := stmtNode.(type) {
	case *ast.DeleteStmt:
		deleteIdx := strings.Index(bindSQL, "DELETE")
		// Remove possible `explain` prefix.
		bindSQL = bindSQL[deleteIdx:]
		return strings.Replace(bindSQL, "DELETE", fmt.Sprintf("DELETE /*+ %s*/", planHint), 1)
	case *ast.UpdateStmt:
		updateIdx := strings.Index(bindSQL, "UPDATE")
		// Remove possible `explain` prefix.
		bindSQL = bindSQL[updateIdx:]
		return strings.Replace(bindSQL, "UPDATE", fmt.Sprintf("UPDATE /*+ %s*/", planHint), 1)
	case *ast.SelectStmt:
		var selectIdx int
		if n.With != nil {
			var withSb strings.Builder
			withIdx := strings.Index(bindSQL, "WITH")
			restoreCtx := format.NewRestoreCtx(format.RestoreStringSingleQuotes|format.RestoreSpacesAroundBinaryOperation|format.RestoreStringWithoutCharset|format.RestoreNameBackQuotes, &withSb)
			restoreCtx.DefaultDB = defaultDB
			if err := n.With.Restore(restoreCtx); err != nil {
				logutil.BgLogger().Debug("restore SQL failed", zap.String("category", "sql-bind"), zap.Error(err))
				return ""
			}
			withEnd := withIdx + len(withSb.String())
			tmp := strings.Replace(bindSQL[withEnd:], "SELECT", fmt.Sprintf("SELECT /*+ %s*/", planHint), 1)
			return strings.Join([]string{bindSQL[withIdx:withEnd], tmp}, "")
		}
		selectIdx = strings.Index(bindSQL, "SELECT")
		// Remove possible `explain` prefix.
		bindSQL = bindSQL[selectIdx:]
		return strings.Replace(bindSQL, "SELECT", fmt.Sprintf("SELECT /*+ %s*/", planHint), 1)
	case *ast.InsertStmt:
		insertIdx := int(0)
		if n.IsReplace {
			insertIdx = strings.Index(bindSQL, "REPLACE")
		} else {
			insertIdx = strings.Index(bindSQL, "INSERT")
		}
		// Remove possible `explain` prefix.
		bindSQL = bindSQL[insertIdx:]
		return strings.Replace(bindSQL, "SELECT", fmt.Sprintf("SELECT /*+ %s*/", planHint), 1)
	}
	logutil.Logger(ctx).Debug("unexpected statement type when generating bind SQL", zap.String("category", "sql-bind"), zap.Any("statement", stmtNode))
	return ""
}

type paramMarkerChecker struct {
	hasParamMarker bool
}

func (e *paramMarkerChecker) Enter(in ast.Node) (ast.Node, bool) {
	if _, ok := in.(*driver.ParamMarkerExpr); ok {
		e.hasParamMarker = true
		return in, true
	}
	return in, false
}

func (*paramMarkerChecker) Leave(in ast.Node) (ast.Node, bool) {
	return in, true
}

// AddEvolvePlanTask adds the evolve plan task into memory cache. It would be flushed to store periodically.
func (h *BindHandle) AddEvolvePlanTask(originalSQL, db string, binding Binding) {
	br := &BindRecord{
		OriginalSQL: originalSQL,
		Db:          db,
		Bindings:    []Binding{binding},
	}
	h.pendingVerifyBindRecordMap.Add(br)
}

// SaveEvolveTasksToStore saves the evolve task into store.
func (h *BindHandle) SaveEvolveTasksToStore() {
	h.pendingVerifyBindRecordMap.flushToStore()
}

func getEvolveParameters(sctx sessionctx.Context) (time.Duration, time.Time, time.Time, error) {
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnBindInfo)
	rows, _, err := sctx.(sqlexec.RestrictedSQLExecutor).ExecRestrictedSQL(
		ctx,
		nil,
		"SELECT variable_name, variable_value FROM mysql.global_variables WHERE variable_name IN (%?, %?, %?)",
		variable.TiDBEvolvePlanTaskMaxTime,
		variable.TiDBEvolvePlanTaskStartTime,
		variable.TiDBEvolvePlanTaskEndTime,
	)
	if err != nil {
		return 0, time.Time{}, time.Time{}, err
	}
	maxTime, startTimeStr, endTimeStr := int64(variable.DefTiDBEvolvePlanTaskMaxTime), variable.DefTiDBEvolvePlanTaskStartTime, variable.DefAutoAnalyzeEndTime
	for _, row := range rows {
		switch row.GetString(0) {
		case variable.TiDBEvolvePlanTaskMaxTime:
			maxTime, err = strconv.ParseInt(row.GetString(1), 10, 64)
			if err != nil {
				return 0, time.Time{}, time.Time{}, err
			}
		case variable.TiDBEvolvePlanTaskStartTime:
			startTimeStr = row.GetString(1)
		case variable.TiDBEvolvePlanTaskEndTime:
			endTimeStr = row.GetString(1)
		}
	}
	startTime, err := time.ParseInLocation(variable.FullDayTimeFormat, startTimeStr, time.UTC)
	if err != nil {
		return 0, time.Time{}, time.Time{}, err
	}
	endTime, err := time.ParseInLocation(variable.FullDayTimeFormat, endTimeStr, time.UTC)
	if err != nil {
		return 0, time.Time{}, time.Time{}, err
	}
	return time.Duration(maxTime) * time.Second, startTime, endTime, nil
}

const (
	// acceptFactor is the factor to decide should we accept the pending verified plan.
	// A pending verified plan will be accepted if it performs at least `acceptFactor` times better than the accepted plans.
	acceptFactor = 1.5
	// verifyTimeoutFactor is how long to wait to verify the pending plan.
	// For debugging purposes it is useful to wait a few times longer than the current execution time so that
	// an informative error can be written to the log.
	verifyTimeoutFactor = 2.0
	// nextVerifyDuration is the duration that we will retry the rejected plans.
	nextVerifyDuration = 7 * 24 * time.Hour
)

func (h *BindHandle) getOnePendingVerifyJob() (originalSQL, db string, binding Binding) {
	cache := h.bindInfo.Value.Load().(*bindCache)
	for _, bindRecord := range cache.GetAllBindRecords() {
		for _, bind := range bindRecord.Bindings {
			if bind.Status == PendingVerify {
				return bindRecord.OriginalSQL, bindRecord.Db, bind
			}
			if bind.Status != Rejected {
				continue
			}
			dur, err := bind.SinceUpdateTime()
			// Should not happen.
			if err != nil {
				continue
			}
			// Rejected and retry it now.
			if dur > nextVerifyDuration {
				return bindRecord.OriginalSQL, bindRecord.Db, bind
			}
		}
	}
	return "", "", Binding{}
}

func (*BindHandle) getRunningDuration(sctx sessionctx.Context, db, sql string, maxTime time.Duration) (time.Duration, error) {
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnBindInfo)
	if db != "" {
		_, err := sctx.(sqlexec.SQLExecutor).ExecuteInternal(ctx, "use %n", db)
		if err != nil {
			return 0, err
		}
	}
	ctx, cancelFunc := context.WithCancel(ctx)
	timer := time.NewTimer(maxTime)
	defer timer.Stop()
	resultChan := make(chan error)
	startTime := time.Now()
	go runSQL(ctx, sctx, sql, resultChan)
	select {
	case err := <-resultChan:
		cancelFunc()
		if err != nil {
			return 0, err
		}
		return time.Since(startTime), nil
	case <-timer.C:
		cancelFunc()
		logutil.BgLogger().Debug("plan verification timed out", zap.String("category", "sql-bind"), zap.Duration("timeElapsed", time.Since(startTime)), zap.String("query", sql))
	}
	<-resultChan
	return -1, nil
}

func runSQL(ctx context.Context, sctx sessionctx.Context, sql string, resultChan chan<- error) {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			stackSize := runtime.Stack(buf, false)
			buf = buf[:stackSize]
			resultChan <- fmt.Errorf("run sql panicked: %v", string(buf))
		}
	}()
	rs, err := sctx.(sqlexec.SQLExecutor).ExecuteInternal(ctx, sql)
	if err != nil {
		if rs != nil {
			terror.Call(rs.Close)
		}
		resultChan <- err
		return
	}
	chk := rs.NewChunk(nil)
	for {
		err = rs.Next(ctx, chk)
		if err != nil || chk.NumRows() == 0 {
			break
		}
	}
	terror.Call(rs.Close)
	resultChan <- err
}

// HandleEvolvePlanTask tries to evolve one plan task.
// It only processes one task at a time because we want each task to use the latest parameters.
func (h *BindHandle) HandleEvolvePlanTask(sctx sessionctx.Context, adminEvolve bool) error {
	originalSQL, db, binding := h.getOnePendingVerifyJob()
	if originalSQL == "" {
		return nil
	}
	maxTime, startTime, endTime, err := getEvolveParameters(sctx)
	if err != nil {
		return err
	}
	if maxTime == 0 || (!timeutil.WithinDayTimePeriod(startTime, endTime, time.Now()) && !adminEvolve) {
		return nil
	}
	sctx.GetSessionVars().UsePlanBaselines = true
	currentPlanTime, err := h.getRunningDuration(sctx, db, binding.BindSQL, maxTime)
	// If we just return the error to the caller, this job will be retried again and again and cause endless logs,
	// since it is still in the bind record. Now we just drop it and if it is actually retryable,
	// we will hope for that we can capture this evolve task again.
	if err != nil {
		_, err = h.DropBindRecord(originalSQL, db, &binding)
		return err
	}
	// If the accepted plan timeouts, it is hard to decide the timeout for verify plan.
	// Currently we simply mark the verify plan as `using` if it could run successfully within maxTime.
	if currentPlanTime > 0 {
		maxTime = time.Duration(float64(currentPlanTime) * verifyTimeoutFactor)
	}
	sctx.GetSessionVars().UsePlanBaselines = false
	verifyPlanTime, err := h.getRunningDuration(sctx, db, binding.BindSQL, maxTime)
	if err != nil {
		_, err = h.DropBindRecord(originalSQL, db, &binding)
		return err
	}
	if verifyPlanTime == -1 || (float64(verifyPlanTime)*acceptFactor > float64(currentPlanTime)) {
		binding.Status = Rejected
		digestText, _ := parser.NormalizeDigest(binding.BindSQL) // for log desensitization
		logutil.BgLogger().Debug("new plan rejected", zap.String("category", "sql-bind"),
			zap.Duration("currentPlanTime", currentPlanTime),
			zap.Duration("verifyPlanTime", verifyPlanTime),
			zap.String("digestText", digestText),
		)
	} else {
		binding.Status = Enabled
	}
	// We don't need to pass the `sctx` because the BindSQL has been validated already.
	return h.AddBindRecord(nil, &BindRecord{OriginalSQL: originalSQL, Db: db, Bindings: []Binding{binding}})
}

// Clear resets the bind handle. It is only used for test.
func (h *BindHandle) Clear() {
	h.bindInfo.Lock()
	h.bindInfo.Store(newBindCache())
	h.bindInfo.lastUpdateTime = types.ZeroTimestamp
	h.bindInfo.Unlock()
	h.invalidBindRecordMap.Store(make(map[string]*bindRecordUpdate))
	h.pendingVerifyBindRecordMap.Store(make(map[string]*bindRecordUpdate))
}

// FlushBindings flushes the BindRecord in temp maps to storage and loads them into cache.
func (h *BindHandle) FlushBindings() error {
	h.DropInvalidBindRecord()
	h.SaveEvolveTasksToStore()
	return h.Update(false)
}

// ReloadBindings clears existing binding cache and do a full load from mysql.bind_info.
// It is used to maintain consistency between cache and mysql.bind_info if the table is deleted or truncated.
func (h *BindHandle) ReloadBindings() error {
	h.bindInfo.Lock()
	h.bindInfo.Store(newBindCache())
	h.bindInfo.lastUpdateTime = types.ZeroTimestamp
	h.bindInfo.Unlock()
	return h.Update(true)
}
