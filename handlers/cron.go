package handlers

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/zangwp/yub-wpanel/database"
	"github.com/zangwp/yub-wpanel/executor"
	"github.com/zangwp/yub-wpanel/i18n"
	"github.com/zangwp/yub-wpanel/models"

	"github.com/gin-gonic/gin"
)

type CronHandler struct{}

// cronMutationMu serializes the database mutation, WordPress Cron side effect,
// and synchronous managed-cron render across both CronHandler and the backup
// policy endpoint.
var cronMutationMu sync.Mutex

var renderManagedCron = executor.RenderCronConfig

func renderCronForRequest(c *gin.Context, db *sql.DB, affectedWPCronSites []int, compensate func() error) bool {
	result := renderManagedCron()
	if result.Success {
		return true
	}
	log.Printf("同步系统Cron失败: %s", result.Message)
	if compensateErr := compensate(); compensateErr != nil {
		reconcileErr := syncWPCronSideEffects(db, affectedWPCronSites...)
		retryResult := renderManagedCron()
		var retryErr error
		if !retryResult.Success {
			retryErr = errors.New(retryResult.Message)
		}
		log.Printf("Cron 同步失败且数据库补偿失败: %v", errors.Join(compensateErr, reconcileErr, retryErr))
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("系统 Cron 更新失败，且数据库补偿失败；请立即检查 Cron 状态"))
		return false
	}
	restoreResult := renderManagedCron()
	if !restoreResult.Success {
		log.Printf("Cron 数据库变更已撤销，但恢复原系统 Cron 失败: %s", restoreResult.Message)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Cron 数据库变更已撤销，但原系统 Cron 恢复失败"))
		return false
	}
	c.JSON(http.StatusInternalServerError, models.ErrorResponse(i18n.TE(c.Request, "cron.system_update_failed")+"，数据库变更已撤销"))
	return false
}

type cronQueryRower interface {
	QueryRow(query string, args ...interface{}) *sql.Row
}

const managedWPCronLine = "define('DISABLE_WP_CRON', true); // YUB WPanel managed WP-Cron"

var wpCronDefinitionRE = regexp.MustCompile(`(?mi)^[\t ]*define\s*\(\s*['"]DISABLE_WP_CRON['"]\s*,\s*(true|false)\s*\)\s*;`)

func syncWPCronSideEffects(q cronQueryRower, siteIDs ...int) error {
	seen := make(map[int]struct{}, len(siteIDs))
	for _, siteID := range siteIDs {
		if siteID <= 0 {
			continue
		}
		if _, ok := seen[siteID]; ok {
			continue
		}
		seen[siteID] = struct{}{}
		var count int
		if err := q.QueryRow("SELECT COUNT(*) FROM cron_jobs WHERE task_type = 'wp_cron' AND site_id = ?", siteID).Scan(&count); err != nil {
			return fmt.Errorf("count WordPress cron jobs for site %d: %w", siteID, err)
		}
		if count > 0 {
			if err := ensureWPCronDisabled(q, siteID); err != nil {
				return err
			}
		} else {
			if err := removeWPCronIfLast(q, siteID); err != nil {
				return err
			}
		}
	}
	return nil
}

// rollbackCronMutation first restores the authoritative database state, then
// semantically reconciles only YUB WPanel's exact marker against that state.
// It deliberately never writes an old byte snapshot over a site owner's
// concurrent wp-config.php edit.
func rollbackCronMutation(db *sql.DB, tx *sql.Tx, siteIDs []int, cause error) error {
	rollbackErr := tx.Rollback()
	reconcileErr := syncWPCronSideEffects(db, siteIDs...)
	return errors.Join(cause, rollbackErr, reconcileErr)
}

func reconcileWPCronAfterCommitFailure(db *sql.DB, siteIDs []int, cause error) error {
	return errors.Join(cause, syncWPCronSideEffects(db, siteIDs...))
}

func loadCronJobMutationSnapshot(db *sql.DB, id int) (cronJobSnapshot, error) {
	var job cronJobSnapshot
	err := db.QueryRow(`SELECT id,name,cron_expression,command,site_id,run_as_user,task_type,backup_mode,
		keep_count,notify_fail,enabled,running,last_run_at,last_status,last_output,created_at,updated_at
		FROM cron_jobs WHERE id=?`, id).Scan(
		&job.ID, &job.Name, &job.CronExpression, &job.Command, &job.SiteID, &job.RunAsUser,
		&job.TaskType, &job.BackupMode, &job.KeepCount, &job.NotifyFail, &job.Enabled, &job.Running,
		&job.LastRunAt, &job.LastStatus, &job.LastOutput, &job.CreatedAt, &job.UpdatedAt,
	)
	return job, err
}

func restoreCronJobMutationSnapshot(tx *sql.Tx, job cronJobSnapshot) error {
	_, err := tx.Exec(`INSERT INTO cron_jobs(id,name,cron_expression,command,site_id,run_as_user,task_type,backup_mode,
		keep_count,notify_fail,enabled,running,last_run_at,last_status,last_output,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name,cron_expression=excluded.cron_expression,command=excluded.command,
			site_id=excluded.site_id,run_as_user=excluded.run_as_user,task_type=excluded.task_type,
			backup_mode=excluded.backup_mode,keep_count=excluded.keep_count,notify_fail=excluded.notify_fail,
			enabled=excluded.enabled,running=excluded.running,last_run_at=excluded.last_run_at,
			last_status=excluded.last_status,last_output=excluded.last_output,
			created_at=excluded.created_at,updated_at=excluded.updated_at`,
		job.ID, job.Name, job.CronExpression, job.Command, job.SiteID, job.RunAsUser, job.TaskType,
		job.BackupMode, job.KeepCount, job.NotifyFail, job.Enabled, job.Running, job.LastRunAt,
		job.LastStatus, job.LastOutput, job.CreatedAt, job.UpdatedAt,
	)
	return err
}

func compensateCronMutation(db *sql.DB, createdID int, previous *cronJobSnapshot, affectedWPCronSites []int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if previous == nil {
		result, err := tx.Exec(`DELETE FROM cron_jobs WHERE id=?`, createdID)
		if err != nil {
			return err
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			return errors.Join(err, fmt.Errorf("created Cron row changed before compensation"))
		}
	} else if err := restoreCronJobMutationSnapshot(tx, *previous); err != nil {
		return err
	}
	if err := syncWPCronSideEffects(tx, affectedWPCronSites...); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *CronHandler) List(c *gin.Context) {
	db := database.GetDB()
	rows, err := db.Query(
		`WITH user_sites AS (
			SELECT system_user, COUNT(*) AS site_count, MIN(id) AS site_id,
			       MIN(domain) AS domain, MIN(status) AS status
			FROM websites WHERE TRIM(system_user) <> '' GROUP BY system_user
		)
		SELECT cj.id, cj.name, cj.cron_expression, cj.command, cj.task_type, cj.backup_mode,
		       cj.keep_count, cj.notify_fail, cj.site_id, cj.run_as_user, cj.enabled, cj.running,
		       cj.last_run_at, cj.last_status, cj.last_output, cj.created_at, cj.updated_at,
		       COALESCE(w.domain, us.domain, ''),
		       CASE
		         WHEN cj.site_id IS NOT NULL AND w.id IS NULL THEN 'ownership_error'
		         WHEN cj.site_id IS NULL AND TRIM(cj.run_as_user) <> '' AND COALESCE(us.site_count, 0) <> 1 THEN 'ownership_error'
		         WHEN COALESCE(w.status, us.status, 'active') <> 'active' THEN COALESCE(w.status, us.status)
		         WHEN EXISTS (SELECT 1 FROM site_migration_locks ml
		                      WHERE ml.site_id=COALESCE(w.id, us.site_id) AND ml.status='active') THEN 'migration_locked'
		         ELSE ''
		       END
		FROM cron_jobs cj
		LEFT JOIN websites w ON w.id=cj.site_id
		LEFT JOIN user_sites us ON cj.site_id IS NULL AND TRIM(cj.run_as_user) <> '' AND us.system_user=cj.run_as_user
		ORDER BY cj.created_at DESC`,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("查询失败"))
		return
	}
	defer rows.Close()

	var jobs []models.CronJob
	for rows.Next() {
		var j models.CronJob
		var enabled, notifyFail, running int
		if err := rows.Scan(&j.ID, &j.Name, &j.CronExpression, &j.Command,
			&j.TaskType, &j.BackupMode, &j.KeepCount, &notifyFail,
			&j.SiteID, &j.RunAsUser, &enabled, &running, &j.LastRunAt, &j.LastStatus,
			&j.LastOutput, &j.CreatedAt, &j.UpdatedAt, &j.RuntimeSiteDomain, &j.RuntimeReason); err != nil {
			continue
		}
		j.Enabled = enabled == 1
		j.NotifyFail = notifyFail == 1
		j.Running = running == 1
		j.RuntimeSuspended = j.RuntimeReason == "paused" || j.RuntimeReason == "migration_locked"
		jobs = append(jobs, j)
	}
	if jobs == nil {
		jobs = []models.CronJob{}
	}

	c.JSON(http.StatusOK, models.SuccessResponse(jobs))
}

func (h *CronHandler) Create(c *gin.Context) {
	var req models.CreateCronRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("参数错误"))
		return
	}

	db := database.GetDB()
	cronMutationMu.Lock()
	defer cronMutationMu.Unlock()
	enabled := 1
	siteID := interface{}(nil)
	if req.SiteID != nil {
		siteID = *req.SiteID
	}

	taskType := req.TaskType
	if taskType == "" {
		taskType = "command"
	}
	if taskType == "file_backup" && req.BackupMode == "" {
		req.BackupMode = "incremental"
	}
	if msg := validateCronInput(req.Name, req.CronExpression, req.Command, taskType, req.BackupMode, req.RunAsUser, req.SiteID); msg != "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse(msg))
		return
	}
	if taskType == "file_backup" && req.SiteID != nil {
		exists, err := fileBackupTaskExists(db, *req.SiteID, req.BackupMode, 0)
		if err != nil {
			log.Printf("检查重复文件备份任务失败: %v", err)
			c.JSON(http.StatusInternalServerError, models.ErrorResponse(i18n.TE(c.Request, "cron.backup_duplicate_check_failed")))
			return
		}
		if exists {
			c.JSON(http.StatusConflict, models.ErrorResponse(i18n.TE(c.Request, "cron.duplicate_backup_task")))
			return
		}
	}
	notifyFail := 0
	if req.NotifyFail {
		notifyFail = 1
	}
	keepCount := req.KeepCount
	if keepCount <= 0 {
		keepCount = 3
	}
	affectedWPCronSites := []int(nil)
	if taskType == "wp_cron" && req.SiteID != nil {
		affectedWPCronSites = append(affectedWPCronSites, *req.SiteID)
	}
	tx, err := db.Begin()
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("创建失败"))
		return
	}
	insertResult, err := tx.Exec(
		`INSERT INTO cron_jobs (name, cron_expression, command, task_type, backup_mode, keep_count, notify_fail, site_id, run_as_user, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.Name, req.CronExpression, req.Command, taskType, req.BackupMode, keepCount, notifyFail, siteID, req.RunAsUser, enabled,
	)
	if err != nil {
		_ = tx.Rollback()
		log.Printf("创建Cron失败: %v", err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("创建失败"))
		return
	}
	createdID64, err := insertResult.LastInsertId()
	if err != nil || createdID64 <= 0 || createdID64 > int64(^uint(0)>>1) {
		_ = tx.Rollback()
		log.Printf("读取新建 Cron ID 失败: %v", err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("创建失败"))
		return
	}
	createdID := int(createdID64)
	if taskType == "wp_cron" && req.SiteID != nil {
		if err := syncWPCronSideEffects(tx, *req.SiteID); err != nil {
			rollbackErr := rollbackCronMutation(db, tx, affectedWPCronSites, err)
			log.Printf("同步 WordPress Cron 状态失败: %v", rollbackErr)
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("更新 WordPress Cron 配置失败，数据库变更已撤销"))
			return
		}
	}
	if err := tx.Commit(); err != nil {
		reconcileErr := reconcileWPCronAfterCommitFailure(db, affectedWPCronSites, err)
		log.Printf("提交新建 Cron 失败: %v", reconcileErr)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("创建失败"))
		return
	}

	if !renderCronForRequest(c, db, affectedWPCronSites, func() error {
		return compensateCronMutation(db, createdID, nil, affectedWPCronSites)
	}) {
		return
	}

	msg := "Cron任务创建成功"
	if taskType == "wp_cron" {
		msg += "，已自动禁用 WordPress 内置伪 Cron"
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": msg}))
}

func (h *CronHandler) Update(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("无效的任务ID"))
		return
	}

	var req models.UpdateCronRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("参数错误"))
		return
	}

	db := database.GetDB()
	cronMutationMu.Lock()
	defer cronMutationMu.Unlock()
	enabled := 1
	running := 0
	var oldTaskType string
	var oldSiteID int
	err = db.QueryRow("SELECT task_type, COALESCE(site_id, 0), enabled, running FROM cron_jobs WHERE id = ?", id).Scan(&oldTaskType, &oldSiteID, &enabled, &running)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, models.ErrorResponse("计划任务不存在"))
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("读取计划任务失败"))
		return
	}
	if running == 1 {
		c.JSON(http.StatusConflict, models.ErrorResponse("任务正在执行中，不能修改"))
		return
	}
	mutationLocks, lockErr := executor.AcquireCronJobMutationLocks([]int{id})
	if lockErr != nil {
		if errors.Is(lockErr, executor.ErrCronJobAlreadyRunning) {
			c.JSON(http.StatusConflict, models.ErrorResponse("任务正在执行中，不能修改"))
		} else {
			log.Printf("取得 Cron 变更锁失败 id=%d: %v", id, lockErr)
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("取得任务变更锁失败"))
		}
		return
	}
	defer mutationLocks.Close()
	previous, err := loadCronJobMutationSnapshot(db, id)
	if err != nil {
		log.Printf("保存 Cron 更新前状态失败 id=%d: %v", id, err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("读取计划任务失败"))
		return
	}
	if req.Enabled != nil {
		enabled = 0
		if *req.Enabled {
			enabled = 1
		}
	}

	taskType := req.TaskType
	if taskType == "" {
		taskType = "command"
	}
	if taskType == "file_backup" && req.BackupMode == "" {
		req.BackupMode = "incremental"
	}
	if msg := validateCronInput(req.Name, req.CronExpression, req.Command, taskType, req.BackupMode, req.RunAsUser, req.SiteID); msg != "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse(msg))
		return
	}
	if taskType == "file_backup" && req.SiteID != nil {
		exists, err := fileBackupTaskExists(db, *req.SiteID, req.BackupMode, id)
		if err != nil {
			log.Printf("检查重复文件备份任务失败: %v", err)
			c.JSON(http.StatusInternalServerError, models.ErrorResponse(i18n.TE(c.Request, "cron.backup_duplicate_check_failed")))
			return
		}
		if exists {
			c.JSON(http.StatusConflict, models.ErrorResponse(i18n.TE(c.Request, "cron.duplicate_backup_task")))
			return
		}
	}
	notifyFail := 0
	if req.NotifyFail != nil && *req.NotifyFail {
		notifyFail = 1
	}
	keepCount := 3
	if req.KeepCount != nil && *req.KeepCount > 0 {
		keepCount = *req.KeepCount
	}

	newSiteID := 0
	if req.SiteID != nil {
		newSiteID = *req.SiteID
	}
	affectedWPCronSites := make([]int, 0, 2)
	if oldTaskType == "wp_cron" {
		affectedWPCronSites = append(affectedWPCronSites, oldSiteID)
	}
	if taskType == "wp_cron" {
		affectedWPCronSites = append(affectedWPCronSites, newSiteID)
	}
	tx, err := db.Begin()
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("更新失败"))
		return
	}
	result, err := tx.Exec(
		`UPDATE cron_jobs SET name = ?, cron_expression = ?, command = ?, task_type = ?,
		 backup_mode = ?, keep_count = ?, notify_fail = ?, site_id = ?,
		 run_as_user = ?, enabled = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		req.Name, req.CronExpression, req.Command, taskType, req.BackupMode, keepCount, notifyFail, req.SiteID, req.RunAsUser, enabled, id,
	)
	if err != nil {
		_ = tx.Rollback()
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("更新失败"))
		return
	}
	if rows, rowsErr := result.RowsAffected(); rowsErr != nil || rows != 1 {
		_ = tx.Rollback()
		c.JSON(http.StatusNotFound, models.ErrorResponse("计划任务不存在"))
		return
	}
	if err := syncWPCronSideEffects(tx, affectedWPCronSites...); err != nil {
		rollbackErr := rollbackCronMutation(db, tx, affectedWPCronSites, err)
		log.Printf("同步 WordPress Cron 状态失败: %v", rollbackErr)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("更新 WordPress Cron 配置失败，数据库变更已撤销"))
		return
	}
	if err := tx.Commit(); err != nil {
		reconcileErr := reconcileWPCronAfterCommitFailure(db, affectedWPCronSites, err)
		log.Printf("提交 Cron 更新失败: %v", reconcileErr)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("更新失败"))
		return
	}

	if !renderCronForRequest(c, db, affectedWPCronSites, func() error {
		return compensateCronMutation(db, 0, &previous, affectedWPCronSites)
	}) {
		return
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Cron任务已更新"}))
}

func (h *CronHandler) Delete(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("无效的任务ID"))
		return
	}

	db := database.GetDB()
	cronMutationMu.Lock()
	defer cronMutationMu.Unlock()
	var taskType string
	var siteID, running int
	err = db.QueryRow("SELECT task_type, COALESCE(site_id, 0), running FROM cron_jobs WHERE id = ?", id).Scan(&taskType, &siteID, &running)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, models.ErrorResponse("计划任务不存在"))
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("读取计划任务失败"))
		return
	}
	if running == 1 {
		c.JSON(http.StatusConflict, models.ErrorResponse("任务正在执行中，不能删除"))
		return
	}
	mutationLocks, lockErr := executor.AcquireCronJobMutationLocks([]int{id})
	if lockErr != nil {
		if errors.Is(lockErr, executor.ErrCronJobAlreadyRunning) {
			c.JSON(http.StatusConflict, models.ErrorResponse("任务正在执行中，不能删除"))
		} else {
			log.Printf("取得 Cron 变更锁失败 id=%d: %v", id, lockErr)
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("取得任务变更锁失败"))
		}
		return
	}
	defer mutationLocks.Close()
	previous, err := loadCronJobMutationSnapshot(db, id)
	if err != nil {
		log.Printf("保存 Cron 删除前状态失败 id=%d: %v", id, err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("读取计划任务失败"))
		return
	}
	wpCronSites := []int(nil)
	if taskType == "wp_cron" {
		wpCronSites = append(wpCronSites, siteID)
	}
	tx, err := db.Begin()
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("删除失败"))
		return
	}
	if _, err := tx.Exec("DELETE FROM cron_jobs WHERE id = ?", id); err != nil {
		_ = tx.Rollback()
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("删除失败"))
		return
	}

	wpCronRestored := false
	if taskType == "wp_cron" && siteID > 0 {
		var remaining int
		if err := tx.QueryRow("SELECT COUNT(*) FROM cron_jobs WHERE task_type = 'wp_cron' AND site_id = ?", siteID).Scan(&remaining); err != nil {
			rollbackErr := rollbackCronMutation(db, tx, wpCronSites, err)
			log.Printf("检查剩余 WordPress Cron 任务失败: %v", rollbackErr)
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("删除失败"))
			return
		}
		wpCronRestored = remaining == 0
		if err := syncWPCronSideEffects(tx, siteID); err != nil {
			rollbackErr := rollbackCronMutation(db, tx, wpCronSites, err)
			log.Printf("同步 WordPress Cron 状态失败: %v", rollbackErr)
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("更新 WordPress Cron 配置失败，数据库变更已撤销"))
			return
		}
	}
	if err := tx.Commit(); err != nil {
		reconcileErr := reconcileWPCronAfterCommitFailure(db, wpCronSites, err)
		log.Printf("提交 Cron 删除失败: %v", reconcileErr)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("删除失败"))
		return
	}

	if !renderCronForRequest(c, db, wpCronSites, func() error {
		return compensateCronMutation(db, 0, &previous, wpCronSites)
	}) {
		return
	}

	msg := "Cron任务已删除"
	if wpCronRestored {
		msg += "，已恢复 WordPress 内置 Cron"
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": msg}))
}

func (h *CronHandler) ViewLogs(c *gin.Context) {
	linesStr := c.DefaultQuery("lines", "100")
	lines := 100
	if n, err := strconv.Atoi(linesStr); err == nil && n > 0 && n <= 500 {
		lines = n
	}

	logFile := "/www/server/panel/logs/cron.log"
	content := tailFile(logFile, lines)
	if content == "" {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"content": "（暂无执行记录）"}))
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"content": content}))
}

func (h *CronHandler) Run(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("无效的任务ID"))
		return
	}

	db := database.GetDB()
	cronMutationMu.Lock()
	var name string
	if err := db.QueryRow("SELECT name FROM cron_jobs WHERE id = ?", id).Scan(&name); errors.Is(err, sql.ErrNoRows) {
		cronMutationMu.Unlock()
		c.JSON(http.StatusNotFound, models.ErrorResponse("计划任务不存在"))
		return
	} else if err != nil {
		cronMutationMu.Unlock()
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("读取计划任务失败"))
		return
	}
	suspended, domain, reason, gateErr := executor.CronJobRuntimeSuspended(id)
	if gateErr != nil {
		cronMutationMu.Unlock()
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("检查任务关联网站状态失败"))
		return
	}
	if suspended {
		if reason == "paused" && c.Query("confirm_paused") == "1" {
			// Explicit administrator confirmation permits a one-off maintenance run.
		} else if reason == "paused" {
			cronMutationMu.Unlock()
			c.JSON(http.StatusConflict, models.ErrorResponse("网站 "+domain+" 已暂停，确认后才能手动执行该任务"))
			return
		} else {
			cronMutationMu.Unlock()
			c.JSON(http.StatusConflict, models.ErrorResponse("关联网站当前不允许运行该任务"))
			return
		}
	}

	dbResult, execErr := db.Exec("UPDATE cron_jobs SET running = 1 WHERE id = ? AND running = 0", id)
	if execErr != nil {
		cronMutationMu.Unlock()
		log.Printf("标记计划任务运行状态失败 id=%d: %v", id, execErr)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("更新任务运行状态失败"))
		return
	}
	n, rowsErr := dbResult.RowsAffected()
	if rowsErr != nil {
		cronMutationMu.Unlock()
		log.Printf("读取计划任务运行状态变更结果失败 id=%d: %v", id, rowsErr)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("更新任务运行状态失败"))
		return
	}
	if n == 0 {
		cronMutationMu.Unlock()
		c.JSON(http.StatusConflict, models.ErrorResponse("任务正在执行中，请稍后再试"))
		return
	}

	payload := &executor.RunCronPayload{JobID: id, Name: name, ConfirmPaused: c.Query("confirm_paused") == "1"}
	task, err := executor.GlobalQueue.EnqueueContext(c.Request.Context(), executor.TaskRunCron, payload)
	if err != nil {
		if _, resetErr := db.Exec("UPDATE cron_jobs SET running = 0 WHERE id = ?", id); resetErr != nil {
			cronMutationMu.Unlock()
			log.Printf("计划任务入队失败后恢复 running 状态失败 id=%d: %v", id, resetErr)
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("任务队列繁忙，且任务状态恢复失败"))
			return
		}
		cronMutationMu.Unlock()
		c.JSON(http.StatusServiceUnavailable, models.ErrorResponse("任务队列繁忙，请稍后重试"))
		return
	}
	cronMutationMu.Unlock()
	result := <-task.ResultCh

	if result.Success {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": result.Message, "output": result.Data}))
	} else {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(result.Message))
	}
}

type systemCronEntry struct {
	Source   string `json:"source"`
	Schedule string `json:"schedule"`
	User     string `json:"user"`
	Command  string `json:"command"`
}

func (h *CronHandler) SystemList(c *gin.Context) {
	var entries []systemCronEntry

	parseCronFile := func(path, source string) {
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 6 {
				continue
			}
			user := ""
			cmdStart := 5
			if source == "/etc/crontab" || strings.HasPrefix(source, "/etc/cron.d/") {
				user = fields[5]
				cmdStart = 6
			}
			if len(fields) < cmdStart+1 {
				continue
			}
			entries = append(entries, systemCronEntry{
				Source:   source,
				Schedule: strings.Join(fields[0:5], " "),
				User:     user,
				Command:  strings.Join(fields[cmdStart:], " "),
			})
		}
	}

	parseCronFile("/etc/crontab", "/etc/crontab")

	if entries, err := os.ReadDir("/etc/cron.d"); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				path := "/etc/cron.d/" + e.Name()
				parseCronFile(path, path)
			}
		}
	}

	parseCronFile("/var/spool/cron/crontabs/root", "root crontab")

	if entries == nil {
		entries = []systemCronEntry{}
	}

	c.JSON(http.StatusOK, models.SuccessResponse(entries))
}

func wpCronConfigPath(q cronQueryRower, siteID int) (string, error) {
	var webRoot string
	if err := q.QueryRow("SELECT web_root FROM websites WHERE id = ?", siteID).Scan(&webRoot); err != nil {
		return "", fmt.Errorf("load WordPress root for site %d: %w", siteID, err)
	}
	webRoot = filepath.Clean(strings.TrimSpace(webRoot))
	if webRoot == "." || !filepath.IsAbs(webRoot) {
		return "", fmt.Errorf("WordPress root for site %d is unsafe", siteID)
	}
	rootInfo, err := os.Lstat(webRoot)
	if err != nil {
		return "", fmt.Errorf("inspect WordPress root for site %d: %w", siteID, err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("WordPress root for site %d is not a regular directory", siteID)
	}
	configPath := filepath.Join(webRoot, "wp-config.php")
	info, err := os.Lstat(configPath)
	if err != nil {
		return "", fmt.Errorf("inspect WordPress config for site %d: %w", siteID, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("WordPress config for site %d is not a regular file", siteID)
	}
	return configPath, nil
}

const maxWPConfigBytes = 8 << 20

func readWPConfigFile(path string) ([]byte, os.FileInfo, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("WordPress config is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxWPConfigBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > maxWPConfigBytes {
		return nil, nil, errors.New("WordPress config exceeds the safe size limit")
	}
	return data, info, nil
}

func copyWPConfigExtendedAttributes(sourcePath, targetPath string) error {
	size, err := syscall.Listxattr(sourcePath, nil)
	if errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list extended attributes: %w", err)
	}
	if size == 0 {
		return nil
	}
	namesBuffer := make([]byte, size)
	size, err = syscall.Listxattr(sourcePath, namesBuffer)
	if err != nil {
		return fmt.Errorf("read extended attribute names: %w", err)
	}
	namesBuffer = namesBuffer[:size]
	for len(namesBuffer) > 0 {
		separator := bytes.IndexByte(namesBuffer, 0)
		if separator < 0 {
			return errors.New("invalid extended attribute list")
		}
		name := string(namesBuffer[:separator])
		namesBuffer = namesBuffer[separator+1:]
		if name == "" {
			continue
		}
		valueSize, err := syscall.Getxattr(sourcePath, name, nil)
		if err != nil {
			return fmt.Errorf("size extended attribute %s: %w", name, err)
		}
		value := make([]byte, valueSize)
		if valueSize > 0 {
			valueSize, err = syscall.Getxattr(sourcePath, name, value)
			if err != nil {
				return fmt.Errorf("read extended attribute %s: %w", name, err)
			}
			value = value[:valueSize]
		}
		if err := syscall.Setxattr(targetPath, name, value, 0); err != nil {
			return fmt.Errorf("copy extended attribute %s: %w", name, err)
		}
	}
	return nil
}

func writeWPConfigAtomically(path string, expectedData, data []byte) error {
	originalData, original, err := readWPConfigFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(originalData, expectedData) {
		return errors.New("WordPress config changed during update")
	}
	stat, ok := original.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("WordPress config ownership is unavailable")
	}
	if stat.Nlink != 1 {
		return errors.New("WordPress config has an unsafe hard-link count")
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".yub-wpanel-wp-cron-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(original.Mode().Perm()); err != nil {
		return err
	}
	if int(stat.Uid) != os.Geteuid() || int(stat.Gid) != os.Getegid() {
		if err := tmp.Chown(int(stat.Uid), int(stat.Gid)); err != nil {
			return err
		}
	}
	if err := copyWPConfigExtendedAttributes(path, tmpPath); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	currentData, current, err := readWPConfigFile(path)
	if err != nil {
		return err
	}
	if !os.SameFile(original, current) || !bytes.Equal(currentData, expectedData) {
		return errors.New("WordPress config changed during update")
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open WordPress config directory: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("sync WordPress config directory: %w", err)
	}
	return dir.Close()
}

func ensureWPCronDisabled(q cronQueryRower, siteID int) error {
	configPath, err := wpCronConfigPath(q, siteID)
	if err != nil {
		return err
	}
	data, _, err := readWPConfigFile(configPath)
	if err != nil {
		return fmt.Errorf("read WordPress config for site %d: %w", siteID, err)
	}
	content := string(data)
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == managedWPCronLine {
			return nil
		}
	}
	if matches := wpCronDefinitionRE.FindAllStringSubmatch(content, -1); len(matches) > 0 {
		if strings.EqualFold(matches[0][1], "true") {
			// This definition belongs to the site owner; leave it untouched both
			// now and when the last managed task is removed.
			return nil
		}
		return errors.New("wp-config.php explicitly enables WP-Cron; refusing to overwrite the site-owned definition")
	}

	insertion := managedWPCronLine + "\n"
	marker := "/* That's all, stop editing!"
	idx := strings.Index(content, marker)
	if idx < 0 {
		marker = "require_once ABSPATH . 'wp-settings.php';"
		idx = strings.Index(content, marker)
	}
	if idx < 0 {
		return errors.New("wp-config.php insertion marker was not found")
	}
	newContent := content[:idx] + insertion + content[idx:]
	if err := writeWPConfigAtomically(configPath, data, []byte(newContent)); err != nil {
		return fmt.Errorf("write WordPress config for site %d: %w", siteID, err)
	}
	return nil
}

func removeWPCronIfLast(q cronQueryRower, siteID int) error {
	configPath, err := wpCronConfigPath(q, siteID)
	if err != nil {
		return err
	}
	data, _, err := readWPConfigFile(configPath)
	if err != nil {
		return fmt.Errorf("read WordPress config for site %d: %w", siteID, err)
	}
	content := string(data)
	managedLineRE := regexp.MustCompile(`(?m)^[\t ]*` + regexp.QuoteMeta(managedWPCronLine) + `[\t ]*(?:\r?\n|$)`)
	updated := managedLineRE.ReplaceAllString(content, "")
	if updated == content {
		return nil
	}
	if err := writeWPConfigAtomically(configPath, data, []byte(updated)); err != nil {
		return fmt.Errorf("write WordPress config for site %d: %w", siteID, err)
	}
	return nil
}

var cronFieldRe = regexp.MustCompile(`^[0-9*/,\-]+$`)
var cronUserRe = regexp.MustCompile(`^(wp|php)_[a-z0-9_]+$`)

func validateCronInput(name, expr, command, taskType, backupMode, runAsUser string, siteID *int) string {
	if hasLineBreak(name) || strings.ContainsAny(name, "\"`$\\") {
		return "任务名称不能包含换行或特殊字符"
	}
	if !validCronExpression(expr) {
		return "Cron 表达式格式不正确"
	}
	switch taskType {
	case "command":
		if siteID != nil {
			return "普通命令任务不能绑定网站"
		}
		if strings.TrimSpace(command) == "" {
			return "请输入要执行的命令"
		}
		if hasLineBreak(command) {
			return "命令不能包含换行"
		}
	case "file_backup":
		if siteID == nil || *siteID <= 0 {
			return "请选择要备份的网站"
		}
		if backupMode != "" && backupMode != "full" && backupMode != "incremental" {
			return "备份模式不正确"
		}
	case "wp_cron":
		if siteID == nil || *siteID <= 0 {
			return "请选择要调用 WP Cron 的网站"
		}
		if hasLineBreak(command) {
			return "WP Cron 目标不能包含换行"
		}
		if !executor.IsValidDomain(command) {
			return "WP Cron 目标必须是有效域名"
		}
	default:
		return "任务类型不正确"
	}
	if runAsUser != "" {
		if !cronUserRe.MatchString(runAsUser) {
			return "运行用户不正确"
		}
		var count int
		database.GetDB().QueryRow("SELECT COUNT(*) FROM websites WHERE system_user = ?", runAsUser).Scan(&count)
		if count == 0 {
			return "运行用户不属于任何站点"
		}
	}
	return ""
}

func validCronExpression(expr string) bool {
	if hasLineBreak(expr) {
		return false
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false
	}
	ranges := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	for i, field := range fields {
		if !validCronField(field, ranges[i][0], ranges[i][1]) {
			return false
		}
	}
	return true
}

func validCronField(field string, min, max int) bool {
	if field == "" || !cronFieldRe.MatchString(field) || strings.Contains(field, "//") {
		return false
	}
	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return false
		}
		base := part
		if strings.Contains(part, "/") {
			parts := strings.Split(part, "/")
			if len(parts) != 2 || parts[1] == "" {
				return false
			}
			step, err := strconv.Atoi(parts[1])
			if err != nil || step < 1 || step > max {
				return false
			}
			base = parts[0]
		}
		if base == "*" {
			continue
		}
		if strings.Contains(base, "-") {
			parts := strings.Split(base, "-")
			if len(parts) != 2 {
				return false
			}
			start, err1 := strconv.Atoi(parts[0])
			end, err2 := strconv.Atoi(parts[1])
			if err1 != nil || err2 != nil || start < min || end > max || start > end {
				return false
			}
			continue
		}
		value, err := strconv.Atoi(base)
		if err != nil || value < min || value > max {
			return false
		}
	}
	return true
}

func hasLineBreak(s string) bool {
	return strings.ContainsAny(s, "\r\n")
}
