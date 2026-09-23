package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRemoveLegacyWPCommandAt_RemovesOldPanelScript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wp")
	legacyContent := "#!/bin/bash\n" + legacyWPCommandMarker + "\n\necho hi\n"
	if err := os.WriteFile(path, []byte(legacyContent), 0755); err != nil {
		t.Fatalf("failed to seed legacy script: %v", err)
	}

	removeLegacyWPCommandAt(path)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected legacy panel script to be removed, stat err = %v", err)
	}
}

func TestRemoveLegacyWPCommandAt_KeepsUnrelatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wp")
	content := "#!/usr/bin/env php\n<?php // WP-CLI\n"
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}

	removeLegacyWPCommandAt(path)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected unrelated WP-CLI-like file to survive, stat err = %v", err)
	}
}

func TestRemoveLegacyWPCommandAt_KeepsSubstringMatchOutsideLineBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wp")
	// The marker appears, but not as an exact, standalone line — should not match.
	content := "#!/bin/bash\necho '" + legacyWPCommandMarker + " extra text'\n"
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}

	removeLegacyWPCommandAt(path)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected non-exact-line match to survive, stat err = %v", err)
	}
}

func TestRemoveLegacyWPCommandAt_KeepsPaddedMarkerLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wp")
	// Padded with leading/trailing whitespace — must not match, to stay in
	// lockstep with install.sh's strict `grep -qx` check.
	content := "#!/bin/bash\n  " + legacyWPCommandMarker + "  \n"
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}

	removeLegacyWPCommandAt(path)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected padded marker line to survive, stat err = %v", err)
	}
}

func TestRemoveLegacyWPCommandAt_MissingFileIsNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")
	removeLegacyWPCommandAt(path) // must not panic
}

func TestEnsurePanelCommandsAt_WritesLowerAndUpperAliases(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		filepath.Join(dir, "lowercase-b"),
		filepath.Join(dir, "uppercase-B"),
	}

	if err := ensurePanelCommandsAt(paths...); err != nil {
		t.Fatalf("ensurePanelCommandsAt failed: %v", err)
	}
	for _, path := range paths {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != panelCommandScript {
			t.Fatalf("unexpected command content at %s", path)
		}
	}
	if !strings.Contains(panelCommandScript, "用法: b <命令>（也可使用大写 B）") {
		t.Fatal("panel command help does not document the B alias")
	}
	if strings.Contains(panelCommandScript, "yubw status") {
		t.Fatal("panel command script still advertises the removed command")
	}
}

func TestPanelCommandScriptHasValidBashSyntax(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	path := filepath.Join(t.TempDir(), "panel-command")
	if err := os.WriteFile(path, []byte(panelCommandScript), 0755); err != nil {
		t.Fatalf("write panel command script: %v", err)
	}
	if output, err := exec.Command(bash, "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("panel command script has invalid Bash syntax: %v\n%s", err, output)
	}
}

func TestEnsurePanelCommandsAt_RefusesUnrelatedCommandWithoutPartialWrite(t *testing.T) {
	dir := t.TempDir()
	occupied := filepath.Join(dir, "lowercase-b")
	other := filepath.Join(dir, "uppercase-B")
	original := []byte("#!/bin/sh\necho unrelated\n")
	if err := os.WriteFile(occupied, original, 0755); err != nil {
		t.Fatalf("seed unrelated command: %v", err)
	}

	if err := ensurePanelCommandsAt(occupied, other); err == nil {
		t.Fatal("expected an occupied command path to be rejected")
	}
	got, err := os.ReadFile(occupied)
	if err != nil {
		t.Fatalf("read unrelated command: %v", err)
	}
	if string(got) != string(original) {
		t.Fatal("unrelated command was modified")
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("second alias was written despite preflight failure: %v", err)
	}
}

func TestEnsurePanelCommandsAt_ReplacesOwnedCommands(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		filepath.Join(dir, "lowercase-b"),
		filepath.Join(dir, "uppercase-B"),
	}
	old := []byte("#!/bin/bash\n" + panelCommandMarker + "\necho old\n")
	for _, path := range paths {
		if err := os.WriteFile(path, old, 0644); err != nil {
			t.Fatalf("seed owned command %s: %v", path, err)
		}
	}

	if err := ensurePanelCommandsAt(paths...); err != nil {
		t.Fatalf("replace owned commands: %v", err)
	}
	for _, path := range paths {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read replaced command %s: %v", path, err)
		}
		if string(got) != panelCommandScript {
			t.Fatalf("owned command %s was not updated", path)
		}
	}
}

func TestRemoveManagedCommandAt_RemovesLegacyYUBWOnlyWhenOwned(t *testing.T) {
	dir := t.TempDir()
	owned := filepath.Join(dir, "owned-yubw")
	unrelated := filepath.Join(dir, "unrelated-yubw")
	if err := os.WriteFile(owned, []byte("#!/bin/bash\n"+legacyYUBWCommandMarker+"\n"), 0755); err != nil {
		t.Fatalf("seed owned legacy command: %v", err)
	}
	if err := os.WriteFile(unrelated, []byte("#!/bin/sh\necho unrelated\n"), 0755); err != nil {
		t.Fatalf("seed unrelated legacy command: %v", err)
	}

	if err := removeManagedCommandAt(owned, legacyYUBWCommandMarker); err != nil {
		t.Fatalf("remove owned legacy command: %v", err)
	}
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Fatalf("owned legacy command was not removed: %v", err)
	}
	if err := removeManagedCommandAt(unrelated, legacyYUBWCommandMarker); err != nil {
		t.Fatalf("inspect unrelated legacy command: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated legacy command should remain: %v", err)
	}
}

func TestMigratePanelCommandsAt_RemovesLegacyOnlyAfterAliasesSucceed(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		filepath.Join(dir, "lowercase-b"),
		filepath.Join(dir, "uppercase-B"),
	}
	legacy := filepath.Join(dir, "legacy-yubw")
	if err := os.WriteFile(legacy, []byte("#!/bin/bash\n"+legacyYUBWCommandMarker+"\n"), 0755); err != nil {
		t.Fatalf("seed legacy command: %v", err)
	}

	if err := migratePanelCommandsAt(paths, legacy); err != nil {
		t.Fatalf("migrate panel commands: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy command was not removed after successful migration: %v", err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("new command %s is missing: %v", path, err)
		}
	}
}

func TestMigratePanelCommandsAt_KeepsLegacyWhenAliasIsOccupied(t *testing.T) {
	dir := t.TempDir()
	occupied := filepath.Join(dir, "lowercase-b")
	other := filepath.Join(dir, "uppercase-B")
	legacy := filepath.Join(dir, "legacy-yubw")
	if err := os.WriteFile(occupied, []byte("#!/bin/sh\necho unrelated\n"), 0755); err != nil {
		t.Fatalf("seed occupied command: %v", err)
	}
	if err := os.WriteFile(legacy, []byte("#!/bin/bash\n"+legacyYUBWCommandMarker+"\n"), 0755); err != nil {
		t.Fatalf("seed legacy command: %v", err)
	}

	if err := migratePanelCommandsAt([]string{occupied, other}, legacy); err == nil {
		t.Fatal("expected migration to reject occupied alias")
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy command must remain after failed migration: %v", err)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("unoccupied alias was written during failed migration: %v", err)
	}
}

func TestWriteFileAtomic_WritesContentAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b")
	content := []byte("#!/bin/bash\necho hi\n")

	if err := writeFileAtomic(path, content, 0755); err != nil {
		t.Fatalf("writeFileAtomic failed: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("content mismatch: got %q, want %q", got, content)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat written file: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0755 {
		t.Fatalf("unexpected permissions: got %v, want 0755", info.Mode().Perm())
	}

	// No leftover temp files in the target directory.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "b" {
		t.Fatalf("unexpected directory contents: %v", entries)
	}
}

func TestWriteFileAtomic_OverwritesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b")
	if err := os.WriteFile(path, []byte("old content"), 0644); err != nil {
		t.Fatalf("failed to seed existing file: %v", err)
	}

	newContent := []byte("new content")
	if err := writeFileAtomic(path, newContent, 0755); err != nil {
		t.Fatalf("writeFileAtomic failed: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}
	if string(got) != string(newContent) {
		t.Fatalf("content mismatch: got %q, want %q", got, newContent)
	}
}
