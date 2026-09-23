package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zangwp/yub-wpanel/database"
	"github.com/zangwp/yub-wpanel/executor"
)

func failCronRenderForTest(t *testing.T) {
	t.Helper()
	old := renderManagedCron
	renderManagedCron = func() executor.TaskResult {
		return executor.TaskResult{Success: false, Message: "test restart failure"}
	}
	t.Cleanup(func() { renderManagedCron = old })
}

func succeedCronRenderForTest(t *testing.T) {
	t.Helper()
	old := renderManagedCron
	renderManagedCron = func() executor.TaskResult {
		return executor.TaskResult{Success: true, Message: "ok"}
	}
	t.Cleanup(func() { renderManagedCron = old })
}

func setCronTestWPConfig(t *testing.T, siteID int, content string) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "wp-config.php")
	if err := os.WriteFile(path, []byte(content), 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := database.GetDB().Exec(`UPDATE websites SET web_root=? WHERE id=?`, root, siteID); err != nil {
		t.Fatal(err)
	}
	return path
}

func cronJSONRequest(t *testing.T, method, target, body string, params gin.Params) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, target, bytes.NewBufferString(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Params = params
	switch method {
	case http.MethodPost:
		new(CronHandler).Create(ctx)
	case http.MethodPut:
		new(CronHandler).Update(ctx)
	case http.MethodDelete:
		new(CronHandler).Delete(ctx)
	}
	return recorder
}

func TestValidateCronInputRejectsSiteBoundCommandTask(t *testing.T) {
	siteID := 1
	msg := validateCronInput("site command", "0 1 * * *", "echo ok", "command", "", "", &siteID)
	if msg == "" {
		t.Fatal("validateCronInput accepted a command task with site_id, want rejection")
	}
}

func setupCronHandlerRuntimeTest(t *testing.T) {
	t.Helper()
	setupBackupOverviewTestDB(t)
	insertBackupPolicySite(t, 1, "cron.example.com")
	if _, err := database.GetDB().Exec(`INSERT INTO cron_jobs
		(id,name,cron_expression,command,task_type,site_id,enabled)
		VALUES(41,'site cron','* * * * *','cron.example.com','wp_cron',1,1)`); err != nil {
		t.Fatal(err)
	}
}

func TestCronListReportsPausedAndMigrationRuntimeStates(t *testing.T) {
	setupCronHandlerRuntimeTest(t)
	db := database.GetDB()
	if _, err := db.Exec(`UPDATE websites SET status='paused' WHERE id=1`); err != nil {
		t.Fatal(err)
	}

	readState := func() map[string]interface{} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		new(CronHandler).List(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("list status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			Data []map[string]interface{} `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || len(response.Data) != 1 {
			t.Fatalf("list response=%s err=%v", recorder.Body.String(), err)
		}
		return response.Data[0]
	}

	paused := readState()
	if paused["runtime_reason"] != "paused" || paused["runtime_suspended"] != true {
		t.Fatalf("paused runtime state=%v", paused)
	}

	if _, err := db.Exec(`UPDATE websites SET status='active' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	// The gate only reads site_id/status. Avoid building an unrelated migration task fixture.
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO site_migration_locks
		(domain,site_id,migration_site_id,direction,status)
		VALUES('cron.example.com',1,'handler_migration_test','source','active')`); err != nil {
		t.Fatal(err)
	}
	migrating := readState()
	if migrating["runtime_reason"] != "migration_locked" || migrating["runtime_suspended"] != true {
		t.Fatalf("migration runtime state=%v", migrating)
	}
}

func TestCronRunCannotConfirmPastMigrationLock(t *testing.T) {
	setupCronHandlerRuntimeTest(t)
	db := database.GetDB()
	// The handler only reads the active lock. Its parent migration row is outside this test.
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO site_migration_locks
		(domain,site_id,migration_site_id,direction,status)
		VALUES('cron.example.com',1,'handler_confirm_test','source','active')`); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/cron/41/run?confirm_paused=1", nil)
	ctx.Params = gin.Params{{Key: "id", Value: "41"}}
	new(CronHandler).Run(ctx)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("migration run status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestCronRunQueueFailureRestoresRunningState(t *testing.T) {
	setupCronHandlerRuntimeTest(t)
	oldQueue := executor.GlobalQueue
	executor.GlobalQueue = nil
	t.Cleanup(func() { executor.GlobalQueue = oldQueue })

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/cron/41/run", nil)
	ctx.Params = gin.Params{{Key: "id", Value: "41"}}
	new(CronHandler).Run(ctx)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("queue failure status=%d, want %d; body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	var running int
	if err := database.GetDB().QueryRow(`SELECT running FROM cron_jobs WHERE id=41`).Scan(&running); err != nil {
		t.Fatal(err)
	}
	if running != 0 {
		t.Fatalf("cron running=%d after queue rejection, want 0", running)
	}
}

func TestCronMutationsRollBackDatabaseWhenRenderFails(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		setupBackupOverviewTestDB(t)
		failCronRenderForTest(t)
		recorder := cronJSONRequest(t, http.MethodPost, "/api/cron", `{"name":"created","cron_expression":"5 1 * * *","command":"echo created","task_type":"command"}`, nil)
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("create status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var count int
		if err := database.GetDB().QueryRow(`SELECT COUNT(*) FROM cron_jobs WHERE name='created'`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("created row count=%d err=%v", count, err)
		}
	})

	t.Run("update", func(t *testing.T) {
		setupBackupOverviewTestDB(t)
		if _, err := database.GetDB().Exec(`INSERT INTO cron_jobs(id,name,cron_expression,command,task_type,enabled) VALUES(51,'old','* * * * *','echo old','command',1)`); err != nil {
			t.Fatal(err)
		}
		failCronRenderForTest(t)
		recorder := cronJSONRequest(t, http.MethodPut, "/api/cron/51", `{"name":"updated","cron_expression":"10 2 * * *","command":"echo updated","task_type":"command"}`, gin.Params{{Key: "id", Value: "51"}})
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("update status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var name string
		if err := database.GetDB().QueryRow(`SELECT name FROM cron_jobs WHERE id=51`).Scan(&name); err != nil || name != "old" {
			t.Fatalf("updated name=%q err=%v", name, err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		setupBackupOverviewTestDB(t)
		if _, err := database.GetDB().Exec(`INSERT INTO cron_jobs(id,name,cron_expression,command,task_type,enabled) VALUES(52,'delete me','* * * * *','echo old','command',1)`); err != nil {
			t.Fatal(err)
		}
		failCronRenderForTest(t)
		recorder := cronJSONRequest(t, http.MethodDelete, "/api/cron/52", "", gin.Params{{Key: "id", Value: "52"}})
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("delete status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var count int
		if err := database.GetDB().QueryRow(`SELECT COUNT(*) FROM cron_jobs WHERE id=52`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("deleted row count=%d err=%v", count, err)
		}
	})
}

func TestCronUpdateCanReenableDisabledTask(t *testing.T) {
	setupBackupOverviewTestDB(t)
	if _, err := database.GetDB().Exec(`INSERT INTO cron_jobs(id,name,cron_expression,command,task_type,enabled)
		VALUES(61,'disabled','* * * * *','echo old','command',0)`); err != nil {
		t.Fatal(err)
	}
	succeedCronRenderForTest(t)
	recorder := cronJSONRequest(t, http.MethodPut, "/api/cron/61",
		`{"name":"enabled","cron_expression":"10 2 * * *","command":"echo enabled","task_type":"command","enabled":true}`,
		gin.Params{{Key: "id", Value: "61"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var enabled int
	if err := database.GetDB().QueryRow(`SELECT enabled FROM cron_jobs WHERE id=61`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 {
		t.Fatalf("enabled=%d, want 1", enabled)
	}
}

func TestWPCronManagedMarkerLifecycle(t *testing.T) {
	const normalConfig = "<?php\n// DISABLE_WP_CRON is documented here only.\n/* That's all, stop editing! Happy publishing. */\n"
	setupBackupOverviewTestDB(t)
	insertBackupPolicySite(t, 1, "cron.example.com")
	configPath := setCronTestWPConfig(t, 1, normalConfig)
	succeedCronRenderForTest(t)

	created := cronJSONRequest(t, http.MethodPost, "/api/cron",
		`{"name":"managed","cron_expression":"5 1 * * *","command":"cron.example.com","task_type":"wp_cron","site_id":1}`, nil)
	if created.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(managedWPCronLine)) || !bytes.Contains(data, []byte("documented here only")) {
		t.Fatalf("managed marker was not inserted safely: %q", data)
	}
	var id int
	if err := database.GetDB().QueryRow(`SELECT id FROM cron_jobs WHERE name='managed'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	idText := strconv.Itoa(id)
	deleted := cronJSONRequest(t, http.MethodDelete, "/api/cron/"+idText, "", gin.Params{{Key: "id", Value: idText}})
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	data, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(managedWPCronLine)) || !bytes.Contains(data, []byte("DISABLE_WP_CRON is documented")) {
		t.Fatalf("managed marker removal touched unrelated content: %q", data)
	}
}

func TestWPCronDeleteReportsRestoreOnlyAfterLastTask(t *testing.T) {
	const normalConfig = "<?php\n/* That's all, stop editing! Happy publishing. */\n"
	setupBackupOverviewTestDB(t)
	insertBackupPolicySite(t, 1, "cron.example.com")
	configPath := setCronTestWPConfig(t, 1, normalConfig)
	succeedCronRenderForTest(t)

	for _, name := range []string{"first", "second"} {
		recorder := cronJSONRequest(t, http.MethodPost, "/api/cron",
			`{"name":"`+name+`","cron_expression":"5 1 * * *","command":"cron.example.com","task_type":"wp_cron","site_id":1}`, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("create %s status=%d body=%s", name, recorder.Code, recorder.Body.String())
		}
	}
	var firstID, secondID int
	if err := database.GetDB().QueryRow(`SELECT id FROM cron_jobs WHERE name='first'`).Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().QueryRow(`SELECT id FROM cron_jobs WHERE name='second'`).Scan(&secondID); err != nil {
		t.Fatal(err)
	}
	firstText := strconv.Itoa(firstID)
	firstDelete := cronJSONRequest(t, http.MethodDelete, "/api/cron/"+firstText, "", gin.Params{{Key: "id", Value: firstText}})
	if firstDelete.Code != http.StatusOK || strings.Contains(firstDelete.Body.String(), "已恢复 WordPress 内置 Cron") {
		t.Fatalf("first delete status=%d body=%s", firstDelete.Code, firstDelete.Body.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil || !bytes.Contains(data, []byte(managedWPCronLine)) {
		t.Fatalf("managed marker removed before last task: %q err=%v", data, err)
	}

	secondText := strconv.Itoa(secondID)
	secondDelete := cronJSONRequest(t, http.MethodDelete, "/api/cron/"+secondText, "", gin.Params{{Key: "id", Value: secondText}})
	if secondDelete.Code != http.StatusOK || !strings.Contains(secondDelete.Body.String(), "已恢复 WordPress 内置 Cron") {
		t.Fatalf("last delete status=%d body=%s", secondDelete.Code, secondDelete.Body.String())
	}
}

func TestWPCronRollbackPreservesConcurrentOwnerEdit(t *testing.T) {
	const normalConfig = "<?php\n/* That's all, stop editing! Happy publishing. */\n"
	setupBackupOverviewTestDB(t)
	insertBackupPolicySite(t, 1, "cron.example.com")
	configPath := setCronTestWPConfig(t, 1, normalConfig)
	db := database.GetDB()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO cron_jobs(name,cron_expression,command,task_type,site_id,enabled)
		VALUES('temporary','5 1 * * *','cron.example.com','wp_cron',1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := syncWPCronSideEffects(tx, 1); err != nil {
		t.Fatal(err)
	}
	managed, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	ownerEdit := append(append([]byte(nil), managed...), []byte("// owner concurrent edit\n")...)
	if err := os.WriteFile(configPath, ownerEdit, 0640); err != nil {
		t.Fatal(err)
	}
	if err := rollbackCronMutation(db, tx, []int{1}, errors.New("forced mutation failure")); err == nil {
		t.Fatal("rollbackCronMutation returned nil, want original failure")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(managedWPCronLine)) || !bytes.Contains(data, []byte("owner concurrent edit")) {
		t.Fatalf("semantic rollback overwrote owner edit: %q", data)
	}
}

func TestWriteWPConfigAtomicallyRejectsStaleExpectedContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wp-config.php")
	if err := os.WriteFile(path, []byte("before\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("owner edit\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := writeWPConfigAtomically(path, []byte("before\n"), []byte("panel edit\n")); err == nil {
		t.Fatal("stale expected content was accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "owner edit\n" {
		t.Fatalf("stale write changed file: %q err=%v", data, err)
	}
}

func TestWPCronLeavesSiteOwnedDefinitionUntouched(t *testing.T) {
	const ownedConfig = "<?php\ndefine( \"DISABLE_WP_CRON\", true ); // site owner\n/* That's all, stop editing! Happy publishing. */\n"
	setupBackupOverviewTestDB(t)
	insertBackupPolicySite(t, 1, "cron.example.com")
	configPath := setCronTestWPConfig(t, 1, ownedConfig)
	succeedCronRenderForTest(t)

	created := cronJSONRequest(t, http.MethodPost, "/api/cron",
		`{"name":"owned","cron_expression":"5 1 * * *","command":"cron.example.com","task_type":"wp_cron","site_id":1}`, nil)
	if created.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var id int
	if err := database.GetDB().QueryRow(`SELECT id FROM cron_jobs WHERE name='owned'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	idText := strconv.Itoa(id)
	deleted := cronJSONRequest(t, http.MethodDelete, "/api/cron/"+idText, "", gin.Params{{Key: "id", Value: idText}})
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil || string(data) != ownedConfig {
		t.Fatalf("site-owned definition changed: %q err=%v", data, err)
	}
}

func TestWPCronConflictRollsBackDatabaseAndConfig(t *testing.T) {
	const conflictingConfig = "<?php\ndefine('DISABLE_WP_CRON', false);\n/* That's all, stop editing! Happy publishing. */\n"
	setupBackupOverviewTestDB(t)
	insertBackupPolicySite(t, 1, "cron.example.com")
	configPath := setCronTestWPConfig(t, 1, conflictingConfig)
	succeedCronRenderForTest(t)

	recorder := cronJSONRequest(t, http.MethodPost, "/api/cron",
		`{"name":"conflict","cron_expression":"5 1 * * *","command":"cron.example.com","task_type":"wp_cron","site_id":1}`, nil)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("create status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var count int
	if err := database.GetDB().QueryRow(`SELECT COUNT(*) FROM cron_jobs WHERE name='conflict'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("conflicting create row count=%d err=%v, want rollback", count, err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil || string(data) != conflictingConfig {
		t.Fatalf("conflicting config changed: %q err=%v", data, err)
	}
}

func TestWPCronRenderFailureRestoresDatabaseAndManagedMarker(t *testing.T) {
	const normalConfig = "<?php\n/* That's all, stop editing! Happy publishing. */\n"
	setupBackupOverviewTestDB(t)
	insertBackupPolicySite(t, 1, "cron.example.com")
	configPath := setCronTestWPConfig(t, 1, normalConfig)
	failCronRenderForTest(t)

	recorder := cronJSONRequest(t, http.MethodPost, "/api/cron",
		`{"name":"render-failure","cron_expression":"5 1 * * *","command":"cron.example.com","task_type":"wp_cron","site_id":1}`, nil)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("create status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var count int
	if err := database.GetDB().QueryRow(`SELECT COUNT(*) FROM cron_jobs WHERE name='render-failure'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed render retained %d Cron rows", count)
	}
	data, err := os.ReadFile(configPath)
	if err != nil || string(data) != normalConfig {
		t.Fatalf("failed render retained WP-Cron marker or overwrote config: %q err=%v", data, err)
	}
}

func TestCronMutationRenderIsSerializedWithoutTaskQueue(t *testing.T) {
	setupBackupOverviewTestDB(t)
	oldQueue := executor.GlobalQueue
	executor.GlobalQueue = nil
	t.Cleanup(func() { executor.GlobalQueue = oldQueue })

	oldRender := renderManagedCron
	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	renderManagedCron = func() executor.TaskResult {
		switch calls.Add(1) {
		case 1:
			close(firstEntered)
			<-releaseFirst
		case 2:
			close(secondEntered)
		}
		return executor.TaskResult{Success: true, Message: "ok"}
	}
	t.Cleanup(func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
		renderManagedCron = oldRender
	})

	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		responses <- cronJSONRequest(t, http.MethodPost, "/api/cron",
			`{"name":"first","cron_expression":"5 1 * * *","command":"echo first","task_type":"command"}`, nil)
	}()
	select {
	case <-firstEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first cron mutation did not reach synchronous render")
	}

	go func() {
		responses <- cronJSONRequest(t, http.MethodPost, "/api/cron",
			`{"name":"second","cron_expression":"10 2 * * *","command":"echo second","task_type":"command"}`, nil)
	}()
	select {
	case <-secondEntered:
		t.Fatal("second cron mutation rendered before the first mutation completed")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseFirst)
	for i := 0; i < 2; i++ {
		select {
		case recorder := <-responses:
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cron mutation did not finish")
		}
	}
}

func TestCronCreateInsertFailureLeavesNoRowOrRender(t *testing.T) {
	setupBackupOverviewTestDB(t)
	if _, err := database.GetDB().Exec(`CREATE TRIGGER reject_test_cron_insert
		AFTER INSERT ON cron_jobs WHEN NEW.name='abort-insert'
		BEGIN SELECT RAISE(ABORT, 'test insert failure'); END`); err != nil {
		t.Fatal(err)
	}

	old := renderManagedCron
	var renderCalls atomic.Int32
	renderManagedCron = func() executor.TaskResult {
		renderCalls.Add(1)
		return executor.TaskResult{Success: true}
	}
	t.Cleanup(func() { renderManagedCron = old })

	recorder := cronJSONRequest(t, http.MethodPost, "/api/cron",
		`{"name":"abort-insert","cron_expression":"5 1 * * *","command":"echo abort","task_type":"command"}`, nil)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var count int
	if err := database.GetDB().QueryRow(`SELECT COUNT(*) FROM cron_jobs WHERE name='abort-insert'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed insert left %d cron rows, want 0", count)
	}
	if got := renderCalls.Load(); got != 0 {
		t.Fatalf("render calls=%d after failed insert, want 0", got)
	}
}
