package tests

import (
	"os"
	"strings"
	"testing"
)

func TestWPUpdateRestorePollingStopsWhenTaskIsGone(t *testing.T) {
	content, err := os.ReadFile("../templates/wp_inventory_panel.html")
	if err != nil {
		t.Fatal(err)
	}
	template := string(content)
	start := strings.Index(template, "async pollUpdateBackupRestore(")
	if start < 0 {
		t.Fatal("restore polling function not found")
	}
	relativeEnd := strings.Index(template[start:], "\n        stopBackupRestorePolling() {")
	if relativeEnd < 0 {
		t.Fatal("restore polling function end not found")
	}
	end := start + relativeEnd
	polling := template[start:end]

	terminalCheck := "[404, 410].includes(error.status)"
	terminalAt := strings.Index(polling, terminalCheck)
	if terminalAt < 0 {
		t.Fatalf("terminal restore check %q is missing", terminalCheck)
	}
	retryRelative := strings.Index(polling[terminalAt:], "this.backupRestoreTimer = setTimeout")
	if retryRelative < 0 {
		t.Fatal("restore retry scheduling is missing")
	}
	retryAt := terminalAt + retryRelative
	if terminalAt > retryAt {
		t.Fatalf("terminal 404/410 check must run before retry scheduling:\n%s", polling)
	}
	for _, required := range []string{
		"this.stopBackupRestorePolling();",
		"this.pages.backups.restoringID = null;",
		"return;",
	} {
		if !strings.Contains(polling[terminalAt:retryAt], required) {
			t.Fatalf("terminal restore handling is missing %q", required)
		}
	}
}
