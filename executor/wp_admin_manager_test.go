package executor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWPAdminManagerPHPSourceParses(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("php unavailable")
	}
	path := filepath.Join(t.TempDir(), "wp-admin-manager.php")
	if err := os.WriteFile(path, []byte("<?php\n"+wpAdminManagerPHPSource), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(php, "-l", path).CombinedOutput(); err != nil {
		t.Fatalf("php -l: %v: %s", err, output)
	}
}

func TestWPAdminManagerPHPSourceKeepsMutationGuardrails(t *testing.T) {
	required := []string{
		"is_multisite()",
		"DB_NAME!==$yub_wpanel_expected_db",
		"$wpdb->prefix!==$yub_wpanel_expected_prefix",
		"in_array('administrator'",
		"apply_filters('pre_user_login',sanitize_user($yub_wpanel_new_login,true))",
		"apply_filters('illegal_user_logins',[])",
		"information_schema.TABLES",
		"strtoupper($yub_wpanel_engine)!=='INNODB'",
		"sanitize_title($yub_wpanel_new_login)",
		"$yub_wpanel_action==='preflight'",
		"add_filter('send_password_change_email','__return_false'",
		"add_filter('send_email_change_email','__return_false'",
		"START TRANSACTION",
		"ROLLBACK",
		"COMMIT",
		"WP_Session_Tokens::get_instance($yub_wpanel_id)->destroy_all()",
		"wp_check_password($yub_wpanel_password",
	}
	for _, token := range required {
		if !strings.Contains(wpAdminManagerPHPSource, token) {
			t.Fatalf("administrator runner is missing guardrail %q", token)
		}
	}
}

func TestWPAdministratorUpdateOmitsEmptyPassword(t *testing.T) {
	raw, err := json.Marshal(WPAdministratorUpdate{UserID: 9, Login: "customer", Email: "customer@example.com", DisplayName: "Customer"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "password") {
		t.Fatalf("empty password should be omitted: %s", raw)
	}
}

func TestWPAdminManagerErrorCode(t *testing.T) {
	err := &WPAdminManagerError{Code: "login_exists"}
	if got := WPAdminManagerErrorCode(err); got != "login_exists" {
		t.Fatalf("code = %q", got)
	}
	if got := WPAdminManagerErrorCode(os.ErrNotExist); got != "" {
		t.Fatalf("unrelated error code = %q", got)
	}
}
