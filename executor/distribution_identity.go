package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zangwp/yub-wpanel/config"
)

const (
	managedPanelBinaryPath       = "/usr/local/bin/yub-wpanel"
	managedPanelBackupPathPrefix = managedPanelBinaryPath + ".bak."
	maxRollbackPlanBytes         = 64 << 10
)

type updateWatchdogIdentitySpec struct {
	DataDir          string
	ConfigPath       string
	PlanPath         string
	BinaryPath       string
	BackupPathPrefix string
	CronPath         string
	ExpectedUID      uint32
}

// ValidateRuntimeDistributionIdentity verifies identities that cannot be
// established from config strings alone. Read-only candidate modes deliberately
// call this only after returning from --info/--repair-config-check.
func ValidateRuntimeDistributionIdentity(binaryPath, cronPath string) error {
	actualPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve running executable: %w", err)
	}
	if err := validateRuntimeExecutableIdentity(binaryPath, actualPath); err != nil {
		return err
	}
	if err := ValidateManagedCronFileIdentity(cronPath); err != nil {
		return fmt.Errorf("cron distribution identity mismatch: %w", err)
	}
	return nil
}

// ValidateUpdateWatchdogDistributionIdentity is the fail-closed exception for
// the update watchdog. The watchdog intentionally runs from the old backup
// binary, so its executable identity is anchored in the root-owned rollback
// plan instead of the canonical installed binary inode.
func ValidateUpdateWatchdogDistributionIdentity(cfg *config.Config, configPath, planPath string) error {
	_, err := loadValidatedUpdateWatchdogPlan(cfg, configPath, planPath)
	return err
}

func loadValidatedUpdateWatchdogPlan(cfg *config.Config, configPath, planPath string) (rollbackPlan, error) {
	if os.Geteuid() != 0 {
		return rollbackPlan{}, fmt.Errorf("update watchdog distribution identity requires root")
	}
	actualPath, err := os.Executable()
	if err != nil {
		return rollbackPlan{}, fmt.Errorf("resolve watchdog executable: %w", err)
	}
	spec, err := productionUpdateWatchdogIdentitySpec(cfg, configPath)
	if err != nil {
		return rollbackPlan{}, err
	}
	return validateUpdateWatchdogDistributionIdentity(cfg, configPath, planPath, actualPath, "/proc/self/exe", spec)
}

func productionUpdateWatchdogIdentitySpec(cfg *config.Config, configPath string) (updateWatchdogIdentitySpec, error) {
	if cfg == nil || configPath == "" || !filepath.IsAbs(configPath) || filepath.Clean(configPath) != configPath || cfg.Panel.DataDir == "" || !filepath.IsAbs(cfg.Panel.DataDir) || filepath.Clean(cfg.Panel.DataDir) != cfg.Panel.DataDir {
		return updateWatchdogIdentitySpec{}, fmt.Errorf("update watchdog config or data path is not an exact absolute path")
	}
	return updateWatchdogIdentitySpec{
		DataDir:          cfg.Panel.DataDir,
		ConfigPath:       configPath,
		PlanPath:         filepath.Join(cfg.Panel.DataDir, "update_rollback.json"),
		BinaryPath:       managedPanelBinaryPath,
		BackupPathPrefix: managedPanelBackupPathPrefix,
		CronPath:         managedCronPath,
		ExpectedUID:      0,
	}, nil
}

func loadTrustedPendingRollbackPlan(cfg *config.Config, configPath string) (rollbackPlan, error) {
	if os.Geteuid() != 0 {
		return rollbackPlan{}, fmt.Errorf("pending rollback plan identity requires root")
	}
	spec, err := productionUpdateWatchdogIdentitySpec(cfg, configPath)
	if err != nil {
		return rollbackPlan{}, err
	}
	if cfg == nil || !exactCleanAbsolutePath(configPath, spec.ConfigPath) || cfg.Panel.DataDir != spec.DataDir || cfg.Systemd.BinaryPath != spec.BinaryPath || cfg.Paths.CronFile != spec.CronPath {
		return rollbackPlan{}, fmt.Errorf("pending rollback plan distribution identity mismatch")
	}
	if err := validateOwnedDirectory(spec.DataDir, spec.ExpectedUID); err != nil {
		return rollbackPlan{}, fmt.Errorf("pending rollback plan data directory is unsafe: %w", err)
	}
	if err := validateOwnedDirectory(filepath.Dir(spec.ConfigPath), spec.ExpectedUID); err != nil {
		return rollbackPlan{}, fmt.Errorf("pending rollback config parent is unsafe: %w", err)
	}
	if err := validateOwnedRegularFile(spec.ConfigPath, spec.ExpectedUID, 0o077); err != nil {
		return rollbackPlan{}, fmt.Errorf("pending rollback config identity is unsafe: %w", err)
	}
	plan, err := readSecureRollbackPlanFile(spec.PlanPath, spec.ExpectedUID)
	if err != nil {
		return rollbackPlan{}, err
	}
	if err := validateRollbackPlanFields(plan, cfg, spec); err != nil {
		return rollbackPlan{}, err
	}
	if err := validateOwnedRegularFile(spec.BinaryPath, spec.ExpectedUID, 0o022); err != nil {
		return rollbackPlan{}, fmt.Errorf("pending rollback installed binary is unsafe: %w", err)
	}
	if err := validateOwnedRegularFile(plan.BackupBinary, spec.ExpectedUID, 0o022); err != nil {
		return rollbackPlan{}, fmt.Errorf("pending rollback backup binary is unsafe: %w", err)
	}
	if err := validateFileSHA256(plan.BackupBinary, plan.BackupSHA256); err != nil {
		return rollbackPlan{}, fmt.Errorf("pending rollback backup hash mismatch: %w", err)
	}
	if err := ValidateManagedCronFileIdentity(spec.CronPath); err != nil {
		return rollbackPlan{}, fmt.Errorf("pending rollback cron identity mismatch: %w", err)
	}
	return plan, nil
}

func validateUpdateWatchdogDistributionIdentity(cfg *config.Config, configPath, planPath, actualPath, runningExecutablePath string, spec updateWatchdogIdentitySpec) (rollbackPlan, error) {
	if cfg == nil {
		return rollbackPlan{}, fmt.Errorf("update watchdog config is nil")
	}
	if !exactCleanAbsolutePath(configPath, spec.ConfigPath) || !exactCleanAbsolutePath(planPath, spec.PlanPath) {
		return rollbackPlan{}, fmt.Errorf("update watchdog config or plan path is not canonical")
	}
	if cfg.Panel.DataDir != spec.DataDir || cfg.Systemd.BinaryPath != spec.BinaryPath || cfg.Paths.CronFile != spec.CronPath {
		return rollbackPlan{}, fmt.Errorf("update watchdog config distribution identity mismatch")
	}
	if err := validateOwnedDirectory(spec.DataDir, spec.ExpectedUID); err != nil {
		return rollbackPlan{}, fmt.Errorf("update watchdog data directory is unsafe: %w", err)
	}
	if err := validateOwnedDirectory(filepath.Dir(spec.ConfigPath), spec.ExpectedUID); err != nil {
		return rollbackPlan{}, fmt.Errorf("update watchdog config parent is unsafe: %w", err)
	}
	if err := validateOwnedRegularFile(spec.ConfigPath, spec.ExpectedUID, 0o077); err != nil {
		return rollbackPlan{}, fmt.Errorf("update watchdog config identity is unsafe: %w", err)
	}

	plan, err := readSecureRollbackPlanFile(spec.PlanPath, spec.ExpectedUID)
	if err != nil {
		return rollbackPlan{}, err
	}
	if err := validateRollbackPlanFields(plan, cfg, spec); err != nil {
		return rollbackPlan{}, err
	}
	if err := validateOwnedRegularFile(spec.BinaryPath, spec.ExpectedUID, 0o022); err != nil {
		return rollbackPlan{}, fmt.Errorf("installed panel binary identity is unsafe: %w", err)
	}
	if err := validateOwnedRegularFile(plan.BackupBinary, spec.ExpectedUID, 0o022); err != nil {
		return rollbackPlan{}, fmt.Errorf("rollback backup binary identity is unsafe: %w", err)
	}
	if err := validateFileSHA256(plan.BackupBinary, plan.BackupSHA256); err != nil {
		return rollbackPlan{}, fmt.Errorf("rollback backup hash mismatch: %w", err)
	}
	backupInfo, err := os.Lstat(plan.BackupBinary)
	if err != nil {
		return rollbackPlan{}, fmt.Errorf("inspect rollback backup binary: %w", err)
	}
	runningInfo, err := os.Stat(runningExecutablePath)
	if err != nil {
		return rollbackPlan{}, fmt.Errorf("inspect watchdog executable: %w", err)
	}
	actualPath = filepath.Clean(strings.TrimSpace(actualPath))
	if actualPath != plan.BackupBinary || !os.SameFile(backupInfo, runningInfo) {
		return rollbackPlan{}, fmt.Errorf("watchdog executable does not match rollback backup inode")
	}
	if err := ValidateManagedCronFileIdentity(spec.CronPath); err != nil {
		return rollbackPlan{}, fmt.Errorf("watchdog cron distribution identity mismatch: %w", err)
	}
	return plan, nil
}

func validateRollbackPlanFields(plan rollbackPlan, cfg *config.Config, spec updateWatchdogIdentitySpec) error {
	if !exactCleanAbsolutePath(plan.PlanPath, spec.PlanPath) || !exactCleanAbsolutePath(plan.ConfigPath, spec.ConfigPath) {
		return fmt.Errorf("rollback plan path identity mismatch")
	}
	if plan.ReadyPath != spec.PlanPath+".ready" || !exactCleanAbsolutePath(plan.ReadyPath, spec.PlanPath+".ready") || !validSHA256Hex(plan.ReadyNonce) {
		return fmt.Errorf("rollback plan ready handshake identity is invalid")
	}
	if !exactBackupPath(plan.BackupBinary, spec.BackupPathPrefix, plan.CurrentVersion) {
		return fmt.Errorf("rollback backup binary path is invalid")
	}
	if !validSHA256Hex(plan.BackupSHA256) {
		return fmt.Errorf("rollback plan binary digest is invalid")
	}
	if !isCanonicalStableTag(plan.CurrentVersion) || !isCanonicalStableTag(plan.TargetVersion) {
		return fmt.Errorf("rollback plan version identity is invalid")
	}
	if CompareVersions(plan.TargetVersion, plan.CurrentVersion) <= 0 {
		return fmt.Errorf("rollback plan target version must be newer than current version")
	}
	if plan.Trigger != "manual" && plan.Trigger != "auto" {
		return fmt.Errorf("rollback plan trigger is invalid")
	}
	if _, err := time.Parse(time.RFC3339, plan.CreatedAt); err != nil {
		return fmt.Errorf("rollback plan timestamp is invalid: %w", err)
	}
	if plan.HealthURL != healthURL(cfg) {
		return fmt.Errorf("rollback plan health URL identity mismatch")
	}
	return nil
}

func exactCleanAbsolutePath(got, want string) bool {
	return got != "" && got == want && filepath.IsAbs(got) && filepath.Clean(got) == got
}

func exactBackupPath(path, prefix, currentVersion string) bool {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) != filepath.Dir(prefix) {
		return false
	}
	expectedPrefix := prefix + currentVersion + "."
	if !strings.HasPrefix(path, expectedPrefix) || len(path) <= len(expectedPrefix) {
		return false
	}
	_, err := time.Parse("20060102-150405.000000000", strings.TrimPrefix(path, expectedPrefix))
	return err == nil
}

func validSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func fileSHA256Hex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func validateFileSHA256(path, expected string) error {
	if !validSHA256Hex(expected) {
		return fmt.Errorf("invalid expected SHA256")
	}
	actual, err := fileSHA256Hex(path)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("SHA256 mismatch")
	}
	return nil
}

func validateOwnedDirectory(path string, expectedUID uint32) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("directory type or permissions are unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != expectedUID {
		return fmt.Errorf("directory ownership is unsafe")
	}
	return nil
}

func validateOwnedRegularFile(path string, expectedUID uint32, forbiddenMode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&forbiddenMode != 0 {
		return fmt.Errorf("file type or permissions are unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != expectedUID || stat.Nlink != 1 {
		return fmt.Errorf("file ownership or link count is unsafe")
	}
	return nil
}

func readSecureRollbackPlanFile(path string, expectedUID uint32) (rollbackPlan, error) {
	if err := validateOwnedDirectory(filepath.Dir(path), expectedUID); err != nil {
		return rollbackPlan{}, fmt.Errorf("rollback plan parent is unsafe: %w", err)
	}
	if err := validateOwnedRegularFile(path, expectedUID, 0o077); err != nil {
		return rollbackPlan{}, fmt.Errorf("rollback plan identity is unsafe: %w", err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		return rollbackPlan{}, fmt.Errorf("inspect rollback plan: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return rollbackPlan{}, fmt.Errorf("open rollback plan: %w", err)
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return rollbackPlan{}, fmt.Errorf("inspect opened rollback plan: %w", err)
	}
	if !os.SameFile(before, after) {
		return rollbackPlan{}, fmt.Errorf("rollback plan changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRollbackPlanBytes+1))
	if err != nil {
		return rollbackPlan{}, fmt.Errorf("read rollback plan: %w", err)
	}
	if len(data) > maxRollbackPlanBytes {
		return rollbackPlan{}, fmt.Errorf("rollback plan is too large")
	}
	var plan rollbackPlan
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return rollbackPlan{}, fmt.Errorf("parse rollback plan: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return rollbackPlan{}, fmt.Errorf("rollback plan contains trailing data")
	}
	return plan, nil
}

func validateRuntimeExecutableIdentity(expectedPath, actualPath string) error {
	expectedPath = filepath.Clean(strings.TrimSpace(expectedPath))
	actualPath = filepath.Clean(strings.TrimSpace(actualPath))
	if expectedPath == "." || actualPath == "." || !filepath.IsAbs(expectedPath) || !filepath.IsAbs(actualPath) {
		return fmt.Errorf("binary distribution identity path is invalid")
	}
	if expectedPath == managedPanelBinaryPath && os.Geteuid() != 0 {
		return fmt.Errorf("canonical binary distribution identity requires root")
	}
	if actualPath != expectedPath {
		return fmt.Errorf("binary distribution identity mismatch: running %s", actualPath)
	}

	expectedInfo, err := os.Lstat(expectedPath)
	if err != nil {
		return fmt.Errorf("inspect managed binary: %w", err)
	}
	if !expectedInfo.Mode().IsRegular() || expectedInfo.Mode()&os.ModeSymlink != 0 || expectedInfo.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("managed binary identity is unsafe")
	}
	expectedStat, ok := expectedInfo.Sys().(*syscall.Stat_t)
	if !ok || expectedStat.Nlink != 1 || expectedStat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("managed binary ownership is unsafe")
	}

	// Comparing /proc/self/exe with the managed path also rejects a deleted or
	// replaced executable whose textual path still looks canonical.
	runningInfo, err := os.Stat("/proc/self/exe")
	if err != nil {
		return fmt.Errorf("inspect running executable: %w", err)
	}
	if !os.SameFile(expectedInfo, runningInfo) {
		return fmt.Errorf("running executable inode differs from managed binary")
	}
	return nil
}
