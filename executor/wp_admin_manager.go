package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/zangwp/yub-wpanel/config"
	"github.com/zangwp/yub-wpanel/models"
)

const wpAdminManagerPHPSource = `
$yub_wpanel_token=getenv('YUB_WPANEL_RUNNER_TOKEN');$yub_wpanel_sent=false;
$yub_wpanel_send=function($ok,$data=[],$code='')use(&$yub_wpanel_sent,$yub_wpanel_token){if($yub_wpanel_sent)return;$yub_wpanel_sent=true;file_put_contents('php://fd/3',json_encode(['token'=>$yub_wpanel_token,'ok'=>$ok,'data'=>$data,'error_code'=>$code],JSON_UNESCAPED_SLASHES));};
register_shutdown_function(function()use(&$yub_wpanel_sent,$yub_wpanel_send){if(!$yub_wpanel_sent){$e=error_get_last();$yub_wpanel_send(false,[],$e?'fatal_error':'no_result');}});
$yub_wpanel_raw=stream_get_contents(STDIN,65537);$yub_wpanel_input=json_decode($yub_wpanel_raw,true);
if(PHP_SAPI!=='cli'||!preg_match('/^[0-9a-f]{32}$/',$yub_wpanel_token)||strlen($yub_wpanel_raw)>65536||!is_array($yub_wpanel_input)){$yub_wpanel_send(false,[],'invalid_input');exit(2);}
$yub_wpanel_root=$yub_wpanel_input['root']??'';$yub_wpanel_action=$yub_wpanel_input['action']??'';
if(!is_string($yub_wpanel_root)||!is_dir($yub_wpanel_root)||realpath($yub_wpanel_root)!==rtrim($yub_wpanel_root,'/')){$yub_wpanel_send(false,[],'invalid_root');exit(2);}
chdir($yub_wpanel_root);ob_start();if(!defined('WP_USE_THEMES'))define('WP_USE_THEMES',false);if(!defined('WP_HTTP_BLOCK_EXTERNAL'))define('WP_HTTP_BLOCK_EXTERNAL',true);require $yub_wpanel_root.'/wp-load.php';ob_end_clean();
global $wpdb;
if(is_multisite()){$yub_wpanel_send(false,[],'multisite_unsupported');exit(1);}
$yub_wpanel_expected_db=$yub_wpanel_input['db_name']??'';$yub_wpanel_expected_prefix=$yub_wpanel_input['table_prefix']??'';
if(!is_string($yub_wpanel_expected_db)||!is_string($yub_wpanel_expected_prefix)||DB_NAME!==$yub_wpanel_expected_db||$wpdb->prefix!==$yub_wpanel_expected_prefix){$yub_wpanel_send(false,[],'database_mismatch');exit(1);}
if($yub_wpanel_action==='list'){
  $yub_wpanel_users=get_users(['role'=>'administrator','orderby'=>'ID','order'=>'ASC']);$yub_wpanel_items=[];
  foreach($yub_wpanel_users as $yub_wpanel_user){$yub_wpanel_items[]=['id'=>(int)$yub_wpanel_user->ID,'login'=>$yub_wpanel_user->user_login,'email'=>$yub_wpanel_user->user_email,'display_name'=>$yub_wpanel_user->display_name,'nicename'=>$yub_wpanel_user->user_nicename];}
  $yub_wpanel_send(true,['administrators'=>$yub_wpanel_items,'site_admin_email'=>(string)get_option('admin_email')]);exit;
}
if($yub_wpanel_action!=='preflight'&&$yub_wpanel_action!=='update'){$yub_wpanel_send(false,[],'invalid_action');exit(2);}
$yub_wpanel_id=(int)($yub_wpanel_input['user_id']??0);$yub_wpanel_user=get_userdata($yub_wpanel_id);
if(!$yub_wpanel_user||!in_array('administrator',(array)$yub_wpanel_user->roles,true)){$yub_wpanel_send(false,[],'administrator_not_found');exit(1);}
$yub_wpanel_old_login=$yub_wpanel_user->user_login;$yub_wpanel_new_login=$yub_wpanel_input['login']??$yub_wpanel_old_login;
if(!is_string($yub_wpanel_new_login)||$yub_wpanel_new_login===''||mb_strlen($yub_wpanel_new_login)>60){$yub_wpanel_send(false,[],'invalid_login');exit(1);}
$yub_wpanel_sanitized=trim(apply_filters('pre_user_login',sanitize_user($yub_wpanel_new_login,true)));if($yub_wpanel_sanitized!==$yub_wpanel_new_login){$yub_wpanel_send(false,[],'invalid_login');exit(1);}
$yub_wpanel_illegal=(array)apply_filters('illegal_user_logins',[]);if(in_array(strtolower($yub_wpanel_new_login),array_map('strtolower',$yub_wpanel_illegal),true)){$yub_wpanel_send(false,[],'invalid_login');exit(1);}
$yub_wpanel_existing=username_exists($yub_wpanel_new_login);if($yub_wpanel_existing&&((int)$yub_wpanel_existing!==$yub_wpanel_id)){$yub_wpanel_send(false,[],'login_exists');exit(1);}
$yub_wpanel_email=$yub_wpanel_input['email']??$yub_wpanel_user->user_email;if(!is_string($yub_wpanel_email)||!is_email($yub_wpanel_email)){$yub_wpanel_send(false,[],'invalid_email');exit(1);}
$yub_wpanel_existing=email_exists($yub_wpanel_email);if($yub_wpanel_existing&&((int)$yub_wpanel_existing!==$yub_wpanel_id)){$yub_wpanel_send(false,[],'email_exists');exit(1);}
$yub_wpanel_display=$yub_wpanel_input['display_name']??$yub_wpanel_user->display_name;if(!is_string($yub_wpanel_display)||trim($yub_wpanel_display)===''||mb_strlen($yub_wpanel_display)>250){$yub_wpanel_send(false,[],'invalid_display_name');exit(1);}
$yub_wpanel_password=$yub_wpanel_input['password']??'';if(!is_string($yub_wpanel_password)||($yub_wpanel_password!==''&&strlen($yub_wpanel_password)<8)||strlen($yub_wpanel_password)>4096){$yub_wpanel_send(false,[],'invalid_password');exit(1);}
$yub_wpanel_sync_nicename=!empty($yub_wpanel_input['sync_nicename']);$yub_wpanel_sync_admin_email=!empty($yub_wpanel_input['sync_admin_email']);$yub_wpanel_destroy_sessions=!empty($yub_wpanel_input['destroy_sessions']);
$yub_wpanel_transactional_tables=[$wpdb->users,$wpdb->usermeta,$wpdb->options];foreach($yub_wpanel_transactional_tables as $yub_wpanel_table){$yub_wpanel_engine=$wpdb->get_var($wpdb->prepare('SELECT ENGINE FROM information_schema.TABLES WHERE TABLE_SCHEMA=%s AND TABLE_NAME=%s',DB_NAME,$yub_wpanel_table));if(!is_string($yub_wpanel_engine)||strtoupper($yub_wpanel_engine)!=='INNODB'){$yub_wpanel_send(false,[],'non_transactional_engine');exit(1);}}
if($yub_wpanel_sync_nicename&&sanitize_title($yub_wpanel_new_login)===''){$yub_wpanel_send(false,[],'invalid_login');exit(1);}
if($yub_wpanel_action==='preflight'){$yub_wpanel_send(true,['validated'=>true]);exit;}
add_filter('send_password_change_email','__return_false',PHP_INT_MAX);add_filter('send_email_change_email','__return_false',PHP_INT_MAX);
$yub_wpanel_transaction=$wpdb->query('START TRANSACTION');if($yub_wpanel_transaction===false){$yub_wpanel_send(false,[],'transaction_failed');exit(1);}
$yub_wpanel_fail=function($code)use($wpdb,$yub_wpanel_send,$yub_wpanel_id,$yub_wpanel_old_login,$yub_wpanel_new_login){$wpdb->query('ROLLBACK');clean_user_cache($yub_wpanel_id);wp_cache_delete($yub_wpanel_old_login,'userlogins');wp_cache_delete($yub_wpanel_new_login,'userlogins');$yub_wpanel_send(false,[],$code);exit(1);};
$yub_wpanel_conflict=$wpdb->get_var($wpdb->prepare("SELECT ID FROM {$wpdb->users} WHERE user_login=%s AND ID<>%d LIMIT 1 FOR UPDATE",$yub_wpanel_new_login,$yub_wpanel_id));if($yub_wpanel_conflict)$yub_wpanel_fail('login_exists');
$yub_wpanel_update=['ID'=>$yub_wpanel_id,'user_email'=>$yub_wpanel_email,'display_name'=>trim($yub_wpanel_display)];
if($yub_wpanel_password!=='')$yub_wpanel_update['user_pass']=$yub_wpanel_password;
if($yub_wpanel_sync_nicename){$yub_wpanel_update['user_nicename']=sanitize_title($yub_wpanel_new_login);$yub_wpanel_update['nickname']=$yub_wpanel_new_login;}
$yub_wpanel_result=wp_update_user($yub_wpanel_update);if(is_wp_error($yub_wpanel_result))$yub_wpanel_fail('user_update_failed');
if($yub_wpanel_new_login!==$yub_wpanel_old_login){$yub_wpanel_changed=$wpdb->update($wpdb->users,['user_login'=>$yub_wpanel_new_login],['ID'=>$yub_wpanel_id],['%s'],['%d']);if($yub_wpanel_changed===false)$yub_wpanel_fail('login_update_failed');}
if($yub_wpanel_sync_admin_email){update_option('admin_email',$yub_wpanel_email);delete_option('new_admin_email');}
if($yub_wpanel_destroy_sessions||$yub_wpanel_password!==''||$yub_wpanel_new_login!==$yub_wpanel_old_login){WP_Session_Tokens::get_instance($yub_wpanel_id)->destroy_all();}
clean_user_cache($yub_wpanel_id);wp_cache_delete($yub_wpanel_old_login,'userlogins');wp_cache_delete($yub_wpanel_new_login,'userlogins');
$yub_wpanel_updated=get_userdata($yub_wpanel_id);if(!$yub_wpanel_updated||$yub_wpanel_updated->user_login!==$yub_wpanel_new_login||$yub_wpanel_updated->user_email!==$yub_wpanel_email)$yub_wpanel_fail('verification_failed');
if($yub_wpanel_password!==''&&!wp_check_password($yub_wpanel_password,$yub_wpanel_updated->user_pass,$yub_wpanel_id))$yub_wpanel_fail('password_verification_failed');
if($wpdb->query('COMMIT')===false)$yub_wpanel_fail('commit_failed');
$yub_wpanel_send(true,['administrator'=>['id'=>(int)$yub_wpanel_updated->ID,'login'=>$yub_wpanel_updated->user_login,'email'=>$yub_wpanel_updated->user_email,'display_name'=>$yub_wpanel_updated->display_name,'nicename'=>$yub_wpanel_updated->user_nicename],'site_admin_email'=>(string)get_option('admin_email')]);`

type WPAdministrator struct {
	ID          int    `json:"id"`
	Login       string `json:"login"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Nicename    string `json:"nicename"`
}

type WPAdministratorList struct {
	Administrators []WPAdministrator `json:"administrators"`
	SiteAdminEmail string            `json:"site_admin_email"`
}

type WPAdministratorResult struct {
	Administrator  WPAdministrator `json:"administrator"`
	SiteAdminEmail string          `json:"site_admin_email"`
}

type WPAdministratorUpdate struct {
	UserID          int    `json:"user_id"`
	Login           string `json:"login"`
	Password        string `json:"password,omitempty"`
	Email           string `json:"email"`
	DisplayName     string `json:"display_name"`
	SyncNicename    bool   `json:"sync_nicename"`
	SyncAdminEmail  bool   `json:"sync_admin_email"`
	DestroySessions bool   `json:"destroy_sessions"`
}

type wpAdminManagerInput struct {
	Action      string `json:"action"`
	Root        string `json:"root"`
	DBName      string `json:"db_name"`
	TablePrefix string `json:"table_prefix"`
	WPAdministratorUpdate
}

type wpAdminManagerEnvelope struct {
	Token     string          `json:"token"`
	OK        bool            `json:"ok"`
	Data      json.RawMessage `json:"data"`
	ErrorCode string          `json:"error_code"`
}

var wpAdminManagerErrorPattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

type WPAdminManagerError struct{ Code string }

func (e *WPAdminManagerError) Error() string { return "administrator runner: " + e.Code }

func WPAdminManagerErrorCode(err error) string {
	var managerErr *WPAdminManagerError
	if errors.As(err, &managerErr) {
		return managerErr.Code
	}
	return ""
}

func ListWPAdministrators(ctx context.Context, site *models.Website) (WPAdministratorList, error) {
	var result WPAdministratorList
	if err := runWPAdminManager(ctx, site, "list", WPAdministratorUpdate{}, &result); err != nil {
		return result, err
	}
	if result.Administrators == nil {
		result.Administrators = []WPAdministrator{}
	}
	return result, nil
}

func UpdateWPAdministrator(ctx context.Context, site *models.Website, update WPAdministratorUpdate) (WPAdministratorResult, error) {
	var result WPAdministratorResult
	if err := runWPAdminManager(ctx, site, "update", update, &result); err != nil {
		return result, err
	}
	return result, nil
}

func PreflightWPAdministratorUpdate(ctx context.Context, site *models.Website, update WPAdministratorUpdate) error {
	var result struct {
		Validated bool `json:"validated"`
	}
	if err := runWPAdminManager(ctx, site, "preflight", update, &result); err != nil {
		return err
	}
	if !result.Validated {
		return errors.New("administrator preflight response invalid")
	}
	return nil
}

func runWPAdminManager(ctx context.Context, site *models.Website, action string, update WPAdministratorUpdate, target interface{}) error {
	if site == nil || site.SiteType != "wordpress" || site.ID <= 0 || site.DBName == "" || !IsValidWPTablePrefix(site.TablePrefix) {
		return errors.New("invalid WordPress site")
	}
	cfg := config.AppConfig
	if cfg == nil {
		return errors.New("panel configuration unavailable")
	}
	runner, err := newDefaultWPCorePHPRunner(cfg.Paths.WWWRoot)
	if err != nil {
		return err
	}
	validated, err := runner.validate(wpCoreUpdateExecution{WebRoot: site.WebRoot, SystemUser: site.SystemUser})
	if err != nil {
		return err
	}
	input := wpAdminManagerInput{Action: action, Root: validated.root, DBName: site.DBName, TablePrefix: site.TablePrefix, WPAdministratorUpdate: update}
	payload, err := json.Marshal(input)
	if err != nil || len(payload) > 64<<10 {
		return errors.New("administrator request too large")
	}
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return err
	}
	token := hex.EncodeToString(tokenBytes)
	args := []string{"-u", validated.user, "--", validated.php, "-d", "open_basedir=" + strings.Join([]string{validated.root, "/tmp", "/usr/share/php"}, ":"), "-d", "disable_functions=" + sitePHPDisabledFunctions(), "-d", "allow_url_include=0", "-d", "display_errors=0", "-d", "memory_limit=256M", "-r", wpAdminManagerPHPSource}
	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(execCtx, validated.runuser, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "HOME=" + validated.home, "USER=" + validated.user, "LOGNAME=" + validated.user, "TMPDIR=/tmp", "YUB_WPANEL_RUNNER_TOKEN=" + token}
	cmd.Stdin = bytes.NewReader(payload)
	stdout, stderr, protocol := newCountingSink(64<<10, false), newCountingSink(64<<10, false), newCountingSink(64<<10, true)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		return err
	}
	defer readPipe.Close()
	cmd.ExtraFiles = []*os.File{writePipe}
	wpInventoryConfigureCommand(cmd)
	if err := cmd.Start(); err != nil {
		writePipe.Close()
		return errors.New("administrator runner start failed")
	}
	_ = writePipe.Close()
	done := make(chan error, 1)
	go func() { _, err := io.Copy(protocol, readPipe); done <- err }()
	waitErr := cmd.Wait()
	copyErr := <-done
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if execCtx.Err() != nil {
		return errors.New("administrator runner timed out")
	}
	_, stdoutExceeded, _ := stdout.snapshot()
	_, stderrExceeded, _ := stderr.snapshot()
	_, protocolExceeded, raw := protocol.snapshot()
	if copyErr != nil || stdoutExceeded || stderrExceeded || protocolExceeded {
		return errors.New("administrator runner output invalid")
	}
	var envelope wpAdminManagerEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Token != token || waitErr != nil || !envelope.OK {
		if envelope.Token == token && wpAdminManagerErrorPattern.MatchString(envelope.ErrorCode) {
			return &WPAdminManagerError{Code: envelope.ErrorCode}
		}
		return errors.New("administrator runner failed")
	}
	if err := json.Unmarshal(envelope.Data, target); err != nil {
		return errors.New("administrator runner response invalid")
	}
	return nil
}
