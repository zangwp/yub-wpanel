package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zangwp/yub-wpanel/config"
)

func TestRuntimeExecutableIdentityAcceptsCurrentRegularBinary(t *testing.T) {
	actual, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	actual, err = filepath.Abs(actual)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeExecutableIdentity(actual, actual); err != nil {
		t.Fatalf("current test executable identity rejected: %v", err)
	}
}

func TestRuntimeExecutableIdentityRejectsDifferentPathAndSymlink(t *testing.T) {
	actual, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "yub-wpanel")
	if err := os.WriteFile(other, []byte("not-the-running-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeExecutableIdentity(other, actual); err == nil {
		t.Fatal("different executable path was accepted")
	}

	link := filepath.Join(t.TempDir(), "linked-yub-wpanel")
	if err := os.Symlink(actual, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := validateRuntimeExecutableIdentity(link, link); err == nil {
		t.Fatal("symlink executable identity was accepted")
	}
}

func TestProductionWatchdogSpecKeepsCustomConfigAndDataPaths(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{}
	cfg.Panel.DataDir = filepath.Join(root, "custom-data")
	configPath := filepath.Join(root, "custom-config.json")
	spec, err := productionUpdateWatchdogIdentitySpec(cfg, configPath)
	if err != nil {
		t.Fatal(err)
	}
	if spec.DataDir != cfg.Panel.DataDir || spec.ConfigPath != configPath || spec.PlanPath != filepath.Join(cfg.Panel.DataDir, "update_rollback.json") {
		t.Fatalf("custom paths were not preserved: %+v", spec)
	}
	if spec.BinaryPath != managedPanelBinaryPath || spec.BackupPathPrefix != managedPanelBackupPathPrefix || spec.CronPath != managedCronPath {
		t.Fatalf("fixed distribution identities changed: %+v", spec)
	}
}

func TestUpdateWatchdogIdentityAnchorsBackupExecutableAndProtectedPlan(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "custom-data")
	cronDir := filepath.Join(root, "cron.d")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cronDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dataDir, "custom-config.json")
	planPath := filepath.Join(dataDir, "update_rollback.json")
	installedBinary := filepath.Join(root, "installed-yub-wpanel")
	backupPrefix := installedBinary + ".bak."
	backupBinary := backupPrefix + "v1.2.3.20260922-010203.000000001"
	cronPath := filepath.Join(cronDir, "yub_wpanel_cron")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installedBinary, []byte("installed-version"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupBinary, []byte("old-version"), 0o755); err != nil {
		t.Fatal(err)
	}
	backupSHA256, err := fileSHA256Hex(backupBinary)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Panel.DataDir = dataDir
	cfg.Panel.Port = 8080
	cfg.Systemd.BinaryPath = installedBinary
	cfg.Paths.CronFile = cronPath
	spec := updateWatchdogIdentitySpec{
		DataDir:          dataDir,
		ConfigPath:       configPath,
		PlanPath:         planPath,
		BinaryPath:       installedBinary,
		BackupPathPrefix: backupPrefix,
		CronPath:         cronPath,
		ExpectedUID:      uint32(os.Geteuid()),
	}
	plan := rollbackPlan{
		CurrentVersion: "v1.2.3",
		TargetVersion:  "v1.2.4",
		BackupBinary:   backupBinary,
		BackupSHA256:   backupSHA256,
		PlanPath:       planPath,
		ReadyPath:      planPath + ".ready",
		ReadyNonce:     strings.Repeat("b", 64),
		ConfigPath:     configPath,
		HealthURL:      healthURL(cfg),
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		Trigger:        "manual",
	}
	writePlan := func() {
		t.Helper()
		if err := writeRollbackPlanFile(planPath, plan); err != nil {
			t.Fatal(err)
		}
	}
	writePlan()

	if _, err := validateUpdateWatchdogDistributionIdentity(cfg, configPath, planPath, backupBinary, backupBinary, spec); err != nil {
		t.Fatalf("valid watchdog identity rejected: %v", err)
	}
	if _, err := validateUpdateWatchdogDistributionIdentity(cfg, configPath, planPath, installedBinary, backupBinary, spec); err == nil || !strings.Contains(err.Error(), "executable") {
		t.Fatalf("different watchdog executable accepted: %v", err)
	}
	if err := os.WriteFile(backupBinary, []byte("tampered-old-version"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := validateUpdateWatchdogDistributionIdentity(cfg, configPath, planPath, backupBinary, backupBinary, spec); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("tampered rollback backup was accepted: %v", err)
	}
	if err := os.WriteFile(backupBinary, []byte("old-version"), 0o755); err != nil {
		t.Fatal(err)
	}
	backupHardlink := filepath.Join(root, "backup-hardlink")
	if err := os.Link(backupBinary, backupHardlink); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := validateUpdateWatchdogDistributionIdentity(cfg, configPath, planPath, backupBinary, backupBinary, spec); err == nil {
		t.Fatal("hard-linked rollback backup was accepted")
	}
	if err := os.Remove(backupHardlink); err != nil {
		t.Fatal(err)
	}

	plan.ConfigPath = filepath.Join(root, "other-config.json")
	writePlan()
	if _, err := validateUpdateWatchdogDistributionIdentity(cfg, configPath, planPath, backupBinary, backupBinary, spec); err == nil {
		t.Fatal("rollback plan with a different config path was accepted")
	}
	plan.ConfigPath = configPath
	writePlan()

	hardlink := filepath.Join(dataDir, "plan-hardlink")
	if err := os.Link(planPath, hardlink); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := validateUpdateWatchdogDistributionIdentity(cfg, configPath, planPath, backupBinary, backupBinary, spec); err == nil {
		t.Fatal("hard-linked rollback plan was accepted")
	}
	if err := os.Remove(hardlink); err != nil {
		t.Fatal(err)
	}

	victim := filepath.Join(dataDir, "victim")
	if err := os.WriteFile(victim, []byte("victim"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(planPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, planPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := validateUpdateWatchdogDistributionIdentity(cfg, configPath, planPath, backupBinary, backupBinary, spec); err == nil {
		t.Fatal("symlink rollback plan was accepted")
	}
}

func TestRollbackPlanRequiresNewerTargetAndExactVersionedBackupName(t *testing.T) {
	cfg := &config.Config{}
	cfg.Panel.Port = 8080
	spec := updateWatchdogIdentitySpec{
		ConfigPath:       "/custom/config.json",
		PlanPath:         "/custom/data/update_rollback.json",
		BackupPathPrefix: managedPanelBackupPathPrefix,
	}
	base := rollbackPlan{
		CurrentVersion: "v1.2.3",
		TargetVersion:  "v1.2.4",
		BackupBinary:   managedPanelBackupPathPrefix + "v1.2.3.20260922-010203.000000001",
		BackupSHA256:   strings.Repeat("a", 64),
		PlanPath:       spec.PlanPath,
		ReadyPath:      spec.PlanPath + ".ready",
		ReadyNonce:     strings.Repeat("b", 64),
		ConfigPath:     spec.ConfigPath,
		HealthURL:      healthURL(cfg),
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		Trigger:        "manual",
	}
	if err := validateRollbackPlanFields(base, cfg, spec); err != nil {
		t.Fatalf("valid rollback plan rejected: %v", err)
	}
	notNewer := base
	notNewer.TargetVersion = notNewer.CurrentVersion
	if err := validateRollbackPlanFields(notNewer, cfg, spec); err == nil {
		t.Fatal("rollback plan with a non-newer target was accepted")
	}
	wrongVersion := base
	wrongVersion.BackupBinary = managedPanelBackupPathPrefix + "v1.2.2.20260922-010203.000000001"
	if err := validateRollbackPlanFields(wrongVersion, cfg, spec); err == nil {
		t.Fatal("rollback plan backup for another version was accepted")
	}
	badTimestamp := base
	badTimestamp.BackupBinary = managedPanelBackupPathPrefix + "v1.2.3.not-a-timestamp"
	if err := validateRollbackPlanFields(badTimestamp, cfg, spec); err == nil {
		t.Fatal("rollback plan backup with an invalid timestamp was accepted")
	}
	wrongReadyPath := base
	wrongReadyPath.ReadyPath = spec.PlanPath + ".other-ready"
	if err := validateRollbackPlanFields(wrongReadyPath, cfg, spec); err == nil {
		t.Fatal("rollback plan with an unbound ready path was accepted")
	}
	invalidReadyNonce := base
	invalidReadyNonce.ReadyNonce = "predictable"
	if err := validateRollbackPlanFields(invalidReadyNonce, cfg, spec); err == nil {
		t.Fatal("rollback plan with an invalid ready nonce was accepted")
	}
}
