<?php
/**
 * YUB WPanel Optimizer — 设置页与开关模块
 *
 * 插件设置页渲染、禁止检测更新/禁止文件编辑等开关的挂载逻辑、
 * 文件锁定后台提示、插件列表页的设置链接。
 */

if (!defined('ABSPATH')) exit;

trait YUBW_Optimizer_Settings_Trait {

    public static function bootstrap() {
		$cfg = self::load_config();
		if (!empty($cfg['disable_application_passwords'])) {
			add_filter('wp_is_application_passwords_available', '__return_false', PHP_INT_MAX);
		}
        if (get_option(self::OPTION_NO_UPDATES, '0') !== '1') {
            return;
        }

        // Run before core's init callback so update checks are not re-scheduled
        // and immediately cleared on every request.
        remove_action('init', 'wp_schedule_update_checks');
        self::suppress_updates();
        self::clear_update_schedules();
    }

    public static function init() {
        add_action('admin_bar_menu', [__CLASS__, 'admin_bar_button'], 100);
        add_action('admin_menu', [__CLASS__, 'settings_page']);
        add_action('admin_enqueue_scripts', [__CLASS__, 'settings_assets']);
        add_action('admin_post_yubw_cache_clear', [__CLASS__, 'handle_clear']);
        add_action('admin_post_yubw_cache_preload', [__CLASS__, 'handle_preload']);
        add_action('admin_post_yubw_cache_preload_stop', [__CLASS__, 'handle_preload_stop']);
        add_action('save_post', [__CLASS__, 'auto_clear'], 99, 1);
        add_action('deleted_post', [__CLASS__, 'auto_clear'], 99, 1);
        add_action('wp_update_comment_count', [__CLASS__, 'auto_comment_clear']);
        add_filter('plugin_action_links_' . plugin_basename(YUBW_OPTIMIZER_PLUGIN_FILE), [__CLASS__, 'action_links']);
        add_action('admin_notices', [__CLASS__, 'clear_notice']);
        add_action('admin_notices', [__CLASS__, 'file_lock_notice']);
        add_action('admin_notices', [__CLASS__, 'companion_update_notice']);
        add_action('wp_ajax_yubw_optimizer_update_companion', [__CLASS__, 'ajax_update_companion']);
        add_action(self::PRELOAD_HOOK, [__CLASS__, 'process_preload_batch']);
        self::maybe_process_preload_tick();
        self::image_optimizer_init();

    }

    public static function suppress_updates() {
        remove_action('admin_notices', 'update_nag', 3);
        remove_action('network_admin_notices', 'update_nag', 3);
        remove_action('wp_version_check', 'wp_version_check');
        remove_action('admin_init', '_maybe_update_core');
        remove_action('admin_init', '_maybe_update_plugins');
        remove_action('admin_init', '_maybe_update_themes');
        remove_action('load-plugins.php', 'wp_update_plugins');
        remove_action('load-update.php', 'wp_update_plugins');
        remove_action('load-themes.php', 'wp_update_themes');
        remove_action('load-update-core.php', 'wp_update_plugins');
        remove_action('load-update-core.php', 'wp_update_themes');
        remove_action('wp_update_plugins', 'wp_update_plugins');
        remove_action('wp_update_themes', 'wp_update_themes');
        add_filter('pre_site_transient_update_core', [__CLASS__, 'suppress_update_transient']);
        add_filter('pre_site_transient_update_plugins', [__CLASS__, 'suppress_update_transient']);
        add_filter('pre_site_transient_update_themes', [__CLASS__, 'suppress_update_transient']);

        add_filter('wp_get_update_data', [__CLASS__, 'filter_update_data'], 10, 2);
    }

    public static function suppress_update_transient() {
        return null;
    }

    public static function clear_update_schedules() {
        foreach (array('wp_version_check', 'wp_update_plugins', 'wp_update_themes') as $hook) {
            if (wp_next_scheduled($hook) !== false) {
                wp_clear_scheduled_hook($hook);
            }
        }
    }

    public static function filter_update_data($data) {
        $data['counts'] = ['total' => 0, 'plugins' => 0, 'themes' => 0, 'wordpress' => 0, 'translations' => 0];
        $data['title']  = '';
        return $data;
    }

    public static function action_links($links) {
        $links[] = '<a href="' . esc_url(admin_url('options-general.php?page=yub-wpanel-optimizer')) . '">' . esc_html__('Settings', 'yub-wpanel-optimizer') . '</a>';
        return $links;
    }

    public static function settings_page() {
        add_options_page('YUB WPanel Optimizer', 'YUB WPanel Optimizer', 'manage_options', 'yub-wpanel-optimizer', [__CLASS__, 'render_settings']);
    }

    public static function settings_assets($hook) {
        if ($hook !== 'settings_page_yub-wpanel-optimizer') {
            return;
        }
        wp_enqueue_style(
            'yubw-optimizer-settings',
            plugin_dir_url(YUBW_OPTIMIZER_PLUGIN_FILE) . 'assets/settings.css',
            ['dashicons'],
            self::VERSION
        );
    }

    private static function companion_update_info($refresh = false) {
        $key = 'yubw_optimizer_companion_update';
        if (!$refresh) {
            $cached = get_transient($key);
            if (is_array($cached)) return $cached;
        }
        $state = self::fetch_panel_state();
        $latest = is_array($state) ? sanitize_text_field($state['companion_latest_version'] ?? '') : '';
        $info = ['latest' => $latest, 'available' => $latest !== '' && version_compare(self::VERSION, $latest, '<')];
        set_transient($key, $info, 5 * MINUTE_IN_SECONDS);
        return $info;
    }

    public static function companion_update_notice() {
        if (!current_user_can('manage_options')) return;
        $info = self::companion_update_info();
        if (empty($info['available'])) return;
        $url = admin_url('options-general.php?page=yub-wpanel-optimizer#yubw-component-update');
        echo '<div class="notice notice-info"><p><strong>' . esc_html__('A new YUB WPanel Optimizer version is available.', 'yub-wpanel-optimizer') . '</strong> ';
        echo esc_html(sprintf(__('Installed: %1$s; available from YUB WPanel: %2$s.', 'yub-wpanel-optimizer'), self::VERSION, $info['latest'])) . ' ';
        echo '<a href="' . esc_url($url) . '">' . esc_html__('Open YUB WPanel Optimizer to update', 'yub-wpanel-optimizer') . '</a></p></div>';
    }

    public static function ajax_update_companion() {
        check_ajax_referer('yubw_optimizer_settings');
        if (!current_user_can('manage_options')) {
            wp_send_json_error(['message' => __('Insufficient permissions', 'yub-wpanel-optimizer')], 403);
        }
        $domain = wp_parse_url(home_url(), PHP_URL_HOST);
        $resp = self::api_request_public('POST', '/api/sites/companion/update', ['domain' => $domain]);
        if (is_wp_error($resp)) wp_send_json_error(['message' => $resp->get_error_message()]);
        $data = json_decode($resp, true);
        if (empty($data['success'])) wp_send_json_error(['message' => $data['message'] ?? __('Update failed', 'yub-wpanel-optimizer')]);
        delete_transient('yubw_optimizer_companion_update');
        wp_send_json_success(['version' => sanitize_text_field($data['data']['version'] ?? '')]);
    }

    public static function render_settings() {
        $showMaintenance = current_user_can('manage_options') && !is_multisite();
        $cfg = self::load_config();
        $panelUrl = self::get_panel_url();
        $apiKey = self::get_api_key();
        $currentDomain = wp_parse_url(home_url(), PHP_URL_HOST);
        $missing = !$panelUrl || !$apiKey;

        $isPost = isset($_POST['yubw_save']);
        $notice = '';

        // 面板同步：GET 时从面板拉取最新状态，POST 时不拉（避免用旧值覆盖表单新值）
        $companionLatestVersion = '';
        if (!$isPost) {
            $panelState = self::fetch_panel_state();
            if ($panelState) {
                $companionLatestVersion = sanitize_text_field($panelState['companion_latest_version'] ?? '');
                update_option(self::OPTION_FCACHE_ENABLED, !empty($panelState['fastcgi_cache_enabled']) ? '1' : '0');
                update_option(self::OPTION_FCACHE_TTL, intval($panelState['fastcgi_cache_ttl'] ?? 300));
                update_option(self::OPTION_NO_UPDATES, !empty($panelState['disable_wp_updates']) ? '1' : '0');
                update_option(self::OPTION_NO_FILE_EDIT, !empty($panelState['disable_file_editing']) ? '1' : '0');
                update_option(self::OPTION_XMLRPC_ENABLED, !empty($panelState['xmlrpc_enabled']) ? '1' : '0');
                update_option(self::OPTION_DISABLE_APPLICATION_PASSWORDS, !empty($panelState['disable_application_passwords']) ? '1' : '0');
                update_option(self::OPTION_WP_DEBUG, !empty($panelState['wp_debug_enabled']) ? '1' : '0');
                update_option(self::OPTION_POST_REVISIONS, $panelState['wp_post_revisions'] ?? -1);
                update_option(self::OPTION_MEMORY_LIMIT, $panelState['wp_memory_limit'] ?? '');
                update_option(self::OPTION_ANOMALY_MONITOR_STATUS, sanitize_key($panelState['anomaly_monitor_status'] ?? 'disabled'));
                update_option(self::OPTION_PASSWORD_RESET_MODE, sanitize_key($panelState['password_reset_mode'] ?? 'allow'));
                self::update_file_lock_state_option($panelState);
            }
        }

        if ($isPost) {
            check_admin_referer('yubw_optimizer_settings');
            // File protection is not a blanket switch for this settings page.
            // Before disabling a current or future feature, determine its real
            // write targets. Keep database, cache, uploads, and panel-managed
            // operations available when the active lock rules allow them; make
            // only controls that write protected paths (currently wp-config.php)
            // read-only. 面向 AI/开发者：禁止因文件保护而整页禁用，必须按实际写入范围判断。
            $fileLockSafeOnly = self::sync_file_lock_state(true);
            $fcacheEnabled  = !empty($_POST['fcache_enabled'])  ? true : false;
            $fcacheTTL      = isset($_POST['fcache_ttl']) ? intval($_POST['fcache_ttl']) : 300;
            $noUpdates      = $fileLockSafeOnly ? get_option(self::OPTION_NO_UPDATES, '0') === '1' : !empty($_POST['no_updates']);
            $noFileEdit     = $fileLockSafeOnly ? get_option(self::OPTION_NO_FILE_EDIT, '0') === '1' : !empty($_POST['no_file_edit']);
            $wpDebug        = $fileLockSafeOnly ? get_option(self::OPTION_WP_DEBUG, '0') === '1' : !empty($_POST['wp_debug']);
            $postRevisions  = $fileLockSafeOnly ? intval(get_option(self::OPTION_POST_REVISIONS, '-1')) : ((isset($_POST['post_revisions']) && $_POST['post_revisions'] !== '') ? intval($_POST['post_revisions']) : -1);
            $memoryLimit    = $fileLockSafeOnly ? get_option(self::OPTION_MEMORY_LIMIT, '') : (isset($_POST['memory_limit']) ? sanitize_text_field($_POST['memory_limit']) : '');
            $preloadEnabled = !empty($_POST['preload_enabled']) ? true : false;
            $preloadLimit   = isset($_POST['preload_limit']) ? intval(wp_unslash($_POST['preload_limit'])) : 100;

            if ($fcacheTTL < 10)  $fcacheTTL = 300;
            if ($fcacheTTL > 86400) $fcacheTTL = 86400;
            $preloadLimit = self::normalize_preload_limit($preloadLimit);

            $imageModeBefore = self::image_optimizer_mode();
            $imageMode = isset($_POST['image_mode']) ? sanitize_key(wp_unslash($_POST['image_mode'])) : self::IMAGE_MODE_OFF;
            if (!self::image_optimizer_env_ready() || !in_array($imageMode, [self::IMAGE_MODE_OFF, self::IMAGE_MODE_OPTIMIZE, self::IMAGE_MODE_WEBP], true)) {
                $imageMode = self::IMAGE_MODE_OFF;
            }
            $imageJpegQuality = self::clamp_image_quality($_POST['image_jpeg_quality'] ?? 85);
            $imageWebpQuality = self::clamp_image_quality($_POST['image_webp_quality'] ?? 82);
            update_option(self::OPTION_IMAGE_MODE, $imageMode);
            update_option(self::OPTION_IMAGE_JPEG_QUALITY, $imageJpegQuality);
            update_option(self::OPTION_IMAGE_WEBP_QUALITY, $imageWebpQuality);
            $switchedToWebp = ($imageMode === self::IMAGE_MODE_WEBP && $imageModeBefore !== self::IMAGE_MODE_WEBP);

            update_option(self::OPTION_FCACHE_ENABLED, $fcacheEnabled ? '1' : '0');
            update_option(self::OPTION_FCACHE_TTL, $fcacheTTL);
            if (!$fileLockSafeOnly) {
                update_option(self::OPTION_NO_UPDATES, $noUpdates ? '1' : '0');
                if ($noUpdates) {
                    self::clear_update_schedules();
                }
                update_option(self::OPTION_NO_FILE_EDIT, $noFileEdit ? '1' : '0');
                update_option(self::OPTION_WP_DEBUG, $wpDebug ? '1' : '0');
                update_option(self::OPTION_POST_REVISIONS, $postRevisions);
                update_option(self::OPTION_MEMORY_LIMIT, $memoryLimit);
            }
            update_option(self::OPTION_PRELOAD_ENABLED, $preloadEnabled ? '1' : '0');
            update_option(self::OPTION_PRELOAD_LIMIT, $preloadLimit);

            $pushed = self::push_optimizer_settings($fcacheEnabled, $fcacheTTL, $noUpdates, $noFileEdit, $wpDebug, $postRevisions, $memoryLimit, $fileLockSafeOnly);
            if ($pushed === true) {
                $noticeText = $fileLockSafeOnly
                    ? __('Available settings were saved. Settings that modify wp-config.php remain unchanged while file protection is enabled.', 'yub-wpanel-optimizer')
                    : __('Settings saved and synced to the panel.', 'yub-wpanel-optimizer');
                $notice = '<div class="yubw-page-notice yubw-page-notice--success"><p>' . esc_html($noticeText) . '</p></div>';
            } else {
                $errMsg = is_wp_error($pushed) ? $pushed->get_error_message() : __('Unknown error', 'yub-wpanel-optimizer');
                $notice = '<div class="yubw-page-notice yubw-page-notice--warning"><p><strong>' . esc_html__('Note:', 'yub-wpanel-optimizer') . '</strong> ' . esc_html__('Settings were saved locally but failed to sync to the panel. Error message:', 'yub-wpanel-optimizer') . ' <code>' . esc_html($errMsg) . '</code></p><p>' . esc_html__('The next time you open this page, state will be pulled from the panel and may overwrite these changes. Please check whether "Verify panel connection" in the plugin settings works.', 'yub-wpanel-optimizer') . '</p></div>';
            }
            if ($switchedToWebp) {
                $notice .= '<div class="yubw-page-notice yubw-page-notice--info"><p><strong>' . esc_html__('WebP mode is enabled.', 'yub-wpanel-optimizer') . '</strong> ' . esc_html__('Newly uploaded JPG/PNG images are converted automatically to smaller WebP files; the originals are no longer kept. The vast majority of sites can switch without any impact; if some email notifications, share cards, or older plugins turn out to need the original format later, the affected WebP images can be converted back to JPG/PNG at any time.', 'yub-wpanel-optimizer') . '</p></div>';
            }
        }

        $fcacheEnabled  = get_option(self::OPTION_FCACHE_ENABLED, '0') === '1';
        $fcacheTTL      = get_option(self::OPTION_FCACHE_TTL, '300');
        $noUpdates      = get_option(self::OPTION_NO_UPDATES, '0') === '1';
        $noFileEdit     = get_option(self::OPTION_NO_FILE_EDIT, '0') === '1';
        $wpDebug        = get_option(self::OPTION_WP_DEBUG, '0') === '1';
        $postRevisions  = intval(get_option(self::OPTION_POST_REVISIONS, '-1'));
        $memoryLimit    = get_option(self::OPTION_MEMORY_LIMIT, '');
        $log            = get_option(self::OPTION_LOG, []);
        $preloadEnabled = get_option(self::OPTION_PRELOAD_ENABLED, '0') === '1';
        $preloadLimit   = self::normalize_preload_limit(get_option(self::OPTION_PRELOAD_LIMIT, 100));
        $preloadStatus  = self::get_preload_status();
        $fileLockEnabled = get_option(self::OPTION_FILE_LOCK_ENABLED, '0') === '1';
        $imageEnvReady    = self::image_optimizer_env_ready();
        $imageMode        = self::image_optimizer_mode();
        $imageJpegQuality = self::clamp_image_quality(get_option(self::OPTION_IMAGE_JPEG_QUALITY, 85));
        $imageWebpQuality = self::clamp_image_quality(get_option(self::OPTION_IMAGE_WEBP_QUALITY, 82));
        $imageSkippedCount = intval(get_option(self::OPTION_IMAGE_SKIPPED_COUNT, 0));
        $xmlrpcEnabled = get_option('yubw_optimizer_xmlrpc_enabled', '0') === '1';
        $applicationPasswordsDisabled = !empty($cfg['disable_application_passwords']);
        $anomalyMonitorStatus = get_option(self::OPTION_ANOMALY_MONITOR_STATUS, 'disabled');
        if (!in_array($anomalyMonitorStatus, ['disabled', 'pending', 'active', 'error'], true)) {
            $anomalyMonitorStatus = 'disabled';
        }
        $passwordResetMode = get_option(self::OPTION_PASSWORD_RESET_MODE, 'allow');
        if (!in_array($passwordResetMode, ['allow', 'admin', 'all'], true)) {
            $passwordResetMode = 'allow';
        }
        $anomalyLabels = [
            'disabled' => __('Not enabled', 'yub-wpanel-optimizer'),
            'pending'  => __('Waiting for first check', 'yub-wpanel-optimizer'),
            'active'   => __('Monitoring active', 'yub-wpanel-optimizer'),
            'error'    => __('Check failed', 'yub-wpanel-optimizer'),
        ];
        $passwordResetLabels = [
            'allow' => __('All users may reset passwords', 'yub-wpanel-optimizer'),
            'admin' => __('Administrators cannot reset passwords', 'yub-wpanel-optimizer'),
            'all'   => __('Password reset disabled for all users', 'yub-wpanel-optimizer'),
        ];
        $phpMemoryLimit = (string) ini_get('memory_limit');
        if ($phpMemoryLimit === '' || $phpMemoryLimit === '-1') {
            $phpMemoryLimit = __('Unlimited', 'yub-wpanel-optimizer');
        }
        if ($companionLatestVersion === '') {
            $updateInfo = self::companion_update_info();
            $companionLatestVersion = $updateInfo['latest'];
        }
        $companionUpdateAvailable = $companionLatestVersion !== '' && version_compare(self::VERSION, $companionLatestVersion, '<');
        ?>
        <div class="wrap yubw-settings">
            <?php
            $pluginVersion = YUB_WPanel_Optimizer::VERSION;
            $imageModeLabel = !$imageEnvReady
                ? __('Environment not ready', 'yub-wpanel-optimizer')
                : ($imageMode === self::IMAGE_MODE_WEBP ? __('WebP conversion', 'yub-wpanel-optimizer') : ($imageMode === self::IMAGE_MODE_OPTIMIZE ? __('Compression', 'yub-wpanel-optimizer') : __('Off', 'yub-wpanel-optimizer')));

            // 预加载状态派生：仅用于展示，不改变任何业务逻辑。
            $preloadRunning = !empty($preloadStatus['running']);
            $preloadQueued  = intval($preloadStatus['queued']);
            $preloadDone    = intval($preloadStatus['done']);
            $preloadFailed  = intval($preloadStatus['failed']);
            if ($preloadRunning) {
                $preloadSummary = sprintf(__('Preload is running; %d URLs remain in the queue.', 'yub-wpanel-optimizer'), $preloadQueued);
            } elseif ($preloadQueued > 0) {
                $preloadSummary = sprintf(__('Preload is queued; %d URLs are waiting to be processed.', 'yub-wpanel-optimizer'), $preloadQueued);
            } else {
                $preloadSummary = __('Preload is idle and no URLs are pending.', 'yub-wpanel-optimizer');
            }
            $stateTone  = $preloadRunning ? 'info' : 'idle';
            $queueTone  = $preloadQueued > 0 ? 'info' : 'idle';
            $doneTone   = $preloadDone > 0 ? 'ok' : 'idle';
            $failedTone = $preloadFailed > 0 ? 'risk' : 'idle';
            ?>
            <header class="yubw-masthead">
                <div class="yubw-masthead__brand">
                    <span class="yubw-masthead__mark" aria-hidden="true"><img src="<?php echo esc_url(plugin_dir_url(YUBW_OPTIMIZER_PLUGIN_FILE) . 'assets/yub-wpanel-logo.png'); ?>" alt=""></span>
                    <div>
                        <p class="yubw-masthead__eyebrow">YUB WPANEL · MANAGED WORDPRESS</p>
                        <h1>YUB WPanel Optimizer</h1>
                        <p class="yubw-masthead__lead"><?php echo esc_html__('Caching, image, and security policies managed centrally by the server panel.', 'yub-wpanel-optimizer'); ?></p>
                    </div>
                </div>
                <div class="yubw-masthead__meta">
                    <div class="yubw-masthead__item">
                        <span class="yubw-masthead__label"><?php echo esc_html__('Managed site', 'yub-wpanel-optimizer'); ?></span>
                        <span class="yubw-masthead__value"><?php echo esc_html($currentDomain); ?></span>
                    </div>
                    <div class="yubw-masthead__item">
                        <span class="yubw-masthead__label"><?php echo esc_html__('Component version', 'yub-wpanel-optimizer'); ?></span>
                        <span class="yubw-masthead__value"><?php echo esc_html($pluginVersion); ?></span>
                    </div>
                    <?php if ($showMaintenance): ?>
                        <button type="button" class="button yubw-masthead__action" data-yubw-maintenance-open>
                            <span class="dashicons dashicons-lock" aria-hidden="true"></span>
                            <?php echo esc_html__('File protection / maintenance', 'yub-wpanel-optimizer'); ?>
                        </button>
                    <?php endif; ?>
                </div>
            </header>

            <div class="yubw-readout" role="group" aria-label="<?php echo esc_attr__('Component status overview', 'yub-wpanel-optimizer'); ?>">
                <div class="yubw-readout__item">
                    <span class="yubw-readout__label"><?php echo esc_html__('Panel connection', 'yub-wpanel-optimizer'); ?></span>
                    <span class="yubw-readout__value <?php echo $missing ? 'is-risk' : 'is-ok'; ?>">
                        <span class="yubw-flag yubw-flag--<?php echo $missing ? 'risk' : 'ok'; ?>" aria-hidden="true"></span>
                        <?php echo $missing ? esc_html__('Not configured', 'yub-wpanel-optimizer') : esc_html__('Connected', 'yub-wpanel-optimizer'); ?>
                    </span>
                </div>
                <div class="yubw-readout__item">
                    <span class="yubw-readout__label"><?php echo esc_html__('File protection', 'yub-wpanel-optimizer'); ?></span>
                    <span class="yubw-readout__value <?php echo $fileLockEnabled ? 'is-ok' : 'is-idle'; ?>">
                        <span class="yubw-flag yubw-flag--<?php echo $fileLockEnabled ? 'ok' : 'idle'; ?>" aria-hidden="true"></span>
                        <?php echo $fileLockEnabled ? esc_html__('Enabled', 'yub-wpanel-optimizer') : esc_html__('Not enabled', 'yub-wpanel-optimizer'); ?>
                    </span>
                </div>
                <div class="yubw-readout__item">
                    <span class="yubw-readout__label"><?php echo esc_html__('FastCGI cache', 'yub-wpanel-optimizer'); ?></span>
                    <span class="yubw-readout__value <?php echo $fcacheEnabled ? 'is-ok' : 'is-idle'; ?>">
                        <span class="yubw-flag yubw-flag--<?php echo $fcacheEnabled ? 'ok' : 'idle'; ?>" aria-hidden="true"></span>
                        <?php echo $fcacheEnabled ? esc_html__('Enabled', 'yub-wpanel-optimizer') : esc_html__('Disabled', 'yub-wpanel-optimizer'); ?>
                    </span>
                </div>
                <div class="yubw-readout__item">
                    <span class="yubw-readout__label"><?php echo esc_html__('Image handling', 'yub-wpanel-optimizer'); ?></span>
                    <span class="yubw-readout__value <?php echo $imageEnvReady && $imageMode !== self::IMAGE_MODE_OFF ? 'is-ok' : 'is-idle'; ?>">
                        <span class="yubw-flag yubw-flag--<?php echo $imageEnvReady && $imageMode !== self::IMAGE_MODE_OFF ? 'ok' : 'idle'; ?>" aria-hidden="true"></span>
                        <?php echo esc_html($imageModeLabel); ?>
                    </span>
                </div>
                <div class="yubw-readout__item">
                    <span class="yubw-readout__label"><?php echo esc_html__('Component updates', 'yub-wpanel-optimizer'); ?></span>
                    <span class="yubw-readout__value <?php echo $companionUpdateAvailable ? 'is-warn' : 'is-ok'; ?>">
                        <span class="yubw-flag yubw-flag--<?php echo $companionUpdateAvailable ? 'warn' : 'ok'; ?>" aria-hidden="true"></span>
                        <?php echo $companionUpdateAvailable ? esc_html(sprintf(__('Version %s available', 'yub-wpanel-optimizer'), $companionLatestVersion)) : esc_html__('Up to date', 'yub-wpanel-optimizer'); ?>
                    </span>
                </div>
            </div>

            <?php if ($companionUpdateAvailable): ?>
                <div class="yubw-component-update" id="yubw-component-update">
                    <div>
                        <strong><?php echo esc_html__('An update is available for this managed plugin.', 'yub-wpanel-optimizer'); ?></strong>
                        <p><?php echo esc_html(sprintf(__('YUB WPanel can update this plugin from version %1$s to %2$s. The update will not change whether the plugin is active.', 'yub-wpanel-optimizer'), self::VERSION, $companionLatestVersion)); ?></p>
                    </div>
                    <button type="button" class="button button-primary" id="yubw-component-update-btn"><?php echo esc_html__('Update now', 'yub-wpanel-optimizer'); ?></button>
                    <div id="yubw-component-update-msg" aria-live="polite"></div>
                </div>
            <?php endif; ?>

            <nav class="yubw-tabs" id="yubw-tabs" aria-label="<?php echo esc_attr__('YUB WPanel Optimizer settings', 'yub-wpanel-optimizer'); ?>">
                <a href="#" class="nav-tab nav-tab-active" data-tab="cache"><span class="dashicons dashicons-performance" aria-hidden="true"></span><?php echo esc_html__('Cache & Performance', 'yub-wpanel-optimizer'); ?></a>
                <a href="#" class="nav-tab" data-tab="image"><span class="dashicons dashicons-format-image" aria-hidden="true"></span><?php echo esc_html__('Image Optimization', 'yub-wpanel-optimizer'); ?></a>
                <a href="#" class="nav-tab" data-tab="security"><span class="dashicons dashicons-shield" aria-hidden="true"></span><?php echo esc_html__('Security & Maintenance', 'yub-wpanel-optimizer'); ?></a>
                <a href="#" class="nav-tab" data-tab="about"><span class="dashicons dashicons-admin-links" aria-hidden="true"></span><?php echo esc_html__('About & Panel Sync', 'yub-wpanel-optimizer'); ?></a>
            </nav>

            <?php if ($notice !== '' || $missing): ?>
                <div class="yubw-page-feedback" aria-live="polite">
                    <?php echo wp_kses_post($notice); ?>
                    <?php if ($missing): ?>
                        <div class="yubw-page-notice yubw-page-notice--error"><p><strong><?php echo esc_html__('Configuration file missing', 'yub-wpanel-optimizer'); ?></strong> — <?php echo esc_html__('Open the site details page for this website in the YUB WPanel and click the "Install companion plugin" button on the WordPress optimization card to complete initialization.', 'yub-wpanel-optimizer'); ?></p></div>
                    <?php endif; ?>
                </div>
            <?php endif; ?>

            <form id="yubw-form" method="post">
                <?php wp_nonce_field('yubw_optimizer_settings'); ?>

                <div class="yubw-tab-panel" data-tab-panel="cache">
                    <section class="yubw-section yubw-section--featured">
                        <header class="yubw-section__head">
                            <div class="yubw-section__titlerow">
                                <h2 class="yubw-section__title"><span class="dashicons dashicons-performance" aria-hidden="true"></span><?php echo esc_html__('FastCGI cache', 'yub-wpanel-optimizer'); ?></h2>
                                <span class="yubw-section__badge"><?php echo esc_html__('Core cache switch', 'yub-wpanel-optimizer'); ?></span>
                            </div>
                            <p class="yubw-section__desc"><?php echo esc_html__('Helps visitors open pages faster and reduces repeated server work for the same content.', 'yub-wpanel-optimizer'); ?></p>
                        </header>
                        <div class="yubw-section__body">
                            <div class="yubw-field">
                                <div class="yubw-field__head">
                                    <label class="yubw-field__title" for="yubw-fcache-enabled"><?php echo esc_html__('FastCGI cache', 'yub-wpanel-optimizer'); ?></label>
                                    <span class="yubw-field__summary"><?php echo esc_html__('Ideal for blogs and business sites that mostly serve public content.', 'yub-wpanel-optimizer'); ?></span>
                                </div>
                                <div class="yubw-field__body">
                                    <label class="yubw-switch">
                                        <input id="yubw-fcache-enabled" name="fcache_enabled" type="checkbox" value="1" <?php checked($fcacheEnabled); ?>>
                                        <span class="yubw-switch__track"><span class="yubw-switch__thumb"></span></span>
                                        <span class="yubw-switch__text"><?php echo esc_html__('Enable FastCGI cache', 'yub-wpanel-optimizer'); ?></span>
                                    </label>
                                    <div class="yubw-hint">
                                        <span class="dashicons dashicons-info" aria-hidden="true"></span>
                                        <div><?php echo esc_html__('The plugin clears the site cache automatically when content or comments change.', 'yub-wpanel-optimizer'); ?></div>
                                    </div>
                                    <details class="yubw-more">
                                        <summary><?php echo esc_html__('Details', 'yub-wpanel-optimizer'); ?></summary>
                                        <div class="yubw-more__body">
                                            <p><?php echo esc_html__('Logged-in users never get cached pages. Common dynamic requests such as shopping carts are also excluded from the cache by server rules.', 'yub-wpanel-optimizer'); ?></p>
                                            <p><?php echo esc_html__('If the front end does not update immediately after saving content, use "Clear Nginx cache" below. If a CDN is also in use, its cache must be cleared at the CDN provider as well.', 'yub-wpanel-optimizer'); ?></p>
                                        </div>
                                    </details>
                                </div>
                            </div>
                        </div>
                    </section>

                    <section class="yubw-section">
                        <header class="yubw-section__head">
                            <h2 class="yubw-section__title"><span class="dashicons dashicons-clock" aria-hidden="true"></span><?php echo esc_html__('Cache lifetime', 'yub-wpanel-optimizer'); ?></h2>
                            <p class="yubw-section__desc"><?php echo esc_html__('How long a cached copy is kept before it is regenerated automatically.', 'yub-wpanel-optimizer'); ?></p>
                        </header>
                        <div class="yubw-section__body">
                            <div class="yubw-field">
                                <div class="yubw-field__head">
                                    <label class="yubw-field__title" for="yubw-fcache-ttl"><?php echo esc_html__('Cache lifetime', 'yub-wpanel-optimizer'); ?></label>
                                    <span class="yubw-field__summary"><?php echo esc_html__('Regenerated automatically when it expires; no manual cleanup is required.', 'yub-wpanel-optimizer'); ?></span>
                                </div>
                                <div class="yubw-field__body">
                                    <input id="yubw-fcache-ttl" name="fcache_ttl" type="number" class="yubw-input yubw-input--num" value="<?php echo esc_attr($fcacheTTL); ?>" min="10" max="86400">
                                    <p class="yubw-field__note"><?php echo sprintf(esc_html__('%1$s300%2$s seconds equal 5 minutes. For most sites, %1$s300–3600%2$s seconds works well.', 'yub-wpanel-optimizer'), '<span class="yubw-key">', '</span>'); ?></p>
                                    <p class="yubw-field__note"><?php echo esc_html__('Longer lifetimes reduce server load; the cache can still be cleared automatically or manually after content updates.', 'yub-wpanel-optimizer'); ?></p>
                                </div>
                            </div>
                        </div>
                    </section>

                    <section class="yubw-section">
                        <header class="yubw-section__head">
                            <h2 class="yubw-section__title"><span class="dashicons dashicons-update" aria-hidden="true"></span><?php echo esc_html__('Cache preload', 'yub-wpanel-optimizer'); ?></h2>
                            <p class="yubw-section__desc"><?php echo esc_html__('After the cache is cleared, the plugin visits commonly used pages so they are already cached for the next visitor.', 'yub-wpanel-optimizer'); ?></p>
                        </header>
                        <div class="yubw-section__body">
                            <div class="yubw-field">
                                <div class="yubw-field__head">
                                    <label class="yubw-field__title" for="yubw-preload-enabled"><?php echo esc_html__('Preload automatically after cache clear', 'yub-wpanel-optimizer'); ?></label>
                                    <span class="yubw-field__summary"><?php echo esc_html__('Processed slowly in the background, never hitting many pages at once.', 'yub-wpanel-optimizer'); ?></span>
                                </div>
                                <div class="yubw-field__body">
                                    <label class="yubw-switch">
                                        <input id="yubw-preload-enabled" name="preload_enabled" type="checkbox" value="1" <?php checked($preloadEnabled); ?>>
                                        <span class="yubw-switch__track"><span class="yubw-switch__thumb"></span></span>
                                        <span class="yubw-switch__text"><?php echo esc_html__('Enable automatic preload', 'yub-wpanel-optimizer'); ?></span>
                                    </label>
                                    <p class="yubw-hint" id="yubw-preload-requires-cache" <?php echo $fcacheEnabled ? 'style="display:none"' : ''; ?>>
                                        <span class="dashicons dashicons-info" aria-hidden="true"></span>
                                        <?php echo sprintf(esc_html__('Preloading only takes effect after the %1$sFastCGI cache%2$s above is enabled.', 'yub-wpanel-optimizer'), '<strong>', '</strong>'); ?>
                                    </p>
                                    <ul class="yubw-points">
                                        <li><span class="yubw-points__label"><?php echo esc_html__('What it does', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('Visits public pages automatically to build their cache.', 'yub-wpanel-optimizer'); ?></li>
                                        <li><span class="yubw-points__label"><?php echo esc_html__('Scope', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('Prioritizes the home page and recently updated public content; it does not crawl the whole site.', 'yub-wpanel-optimizer'); ?></li>
                                        <li><span class="yubw-points__label"><?php echo esc_html__('Other pages', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('These pages are cached the first time someone visits them.', 'yub-wpanel-optimizer'); ?></li>
                                    </ul>
                                </div>
                            </div>
                            <div class="yubw-field">
                                <div class="yubw-field__head">
                                    <label class="yubw-field__title" for="yubw-preload-limit"><?php echo esc_html__('Max URLs per preload run', 'yub-wpanel-optimizer'); ?></label>
                                    <span class="yubw-field__summary"><?php echo esc_html__('The maximum number of URLs processed in a single preload run.', 'yub-wpanel-optimizer'); ?></span>
                                </div>
                                <div class="yubw-field__body">
                                    <input id="yubw-preload-limit" name="preload_limit" type="number" class="yubw-input yubw-input--num" value="<?php echo esc_attr($preloadLimit); ?>" min="10" max="500">
                                    <p class="yubw-field__note"><?php echo sprintf(esc_html__('Range %1$s10–500%2$s; %1$s100%2$s is a common choice. The home page comes first, followed by recently updated public posts, pages, and term archives.', 'yub-wpanel-optimizer'), '<span class="yubw-key">', '</span>'); ?></p>
                                </div>
                            </div>
                        </div>
                    </section>
                </div>

                <div class="yubw-tab-panel" data-tab-panel="image" style="display:none">
                    <?php if (!$imageEnvReady): ?>
                        <div class="yubw-section">
                            <div class="yubw-section__body">
                                <div class="yubw-page-notice yubw-page-notice--error" style="margin-top:16px"><p><strong><?php echo esc_html__('Image processing is unavailable: the server is missing the exif extension.', 'yub-wpanel-optimizer'); ?></strong> <?php echo sprintf(esc_html__('This feature relies on the PHP %1$sexif%2$s extension to correct photo orientation. Once the panel has installed it, refresh this page to start using it.', 'yub-wpanel-optimizer'), '<code>', '</code>'); ?></p></div>
                            </div>
                        </div>
                    <?php endif; ?>
                    <section class="yubw-section">
                        <header class="yubw-section__head">
                            <h2 class="yubw-section__title"><span class="dashicons dashicons-upload" aria-hidden="true"></span><?php echo esc_html__('Image handling for new uploads', 'yub-wpanel-optimizer'); ?></h2>
                            <p class="yubw-section__desc"><?php echo esc_html__('Chooses how newly uploaded images are processed; published content is not affected.', 'yub-wpanel-optimizer'); ?></p>
                        </header>
                        <div class="yubw-section__body">
                            <div class="yubw-field">
                                <div class="yubw-field__head">
                                    <span class="yubw-field__title"><?php echo esc_html__('Processing mode', 'yub-wpanel-optimizer'); ?></span>
                                    <span class="yubw-field__summary"><?php echo esc_html__('Compress or convert on upload to reduce file size.', 'yub-wpanel-optimizer'); ?></span>
                                </div>
                                <div class="yubw-field__body">
                                    <div class="yubw-choice">
                                        <label class="yubw-choice__item">
                                            <input id="yubw-image-mode-off" type="radio" name="image_mode" value="off" <?php checked($imageMode, self::IMAGE_MODE_OFF); ?> <?php disabled(!$imageEnvReady); ?>>
                                            <span class="yubw-choice__text">
                                                <span class="yubw-choice__label"><?php echo esc_html__('Off', 'yub-wpanel-optimizer'); ?></span>
                                                <span class="yubw-choice__desc"><?php echo esc_html__('Keep the default WordPress behavior: no conversion or compression on upload.', 'yub-wpanel-optimizer'); ?></span>
                                            </span>
                                        </label>
                                        <label class="yubw-choice__item">
                                            <input id="yubw-image-mode-optimize" type="radio" name="image_mode" value="optimize" <?php checked($imageMode, self::IMAGE_MODE_OPTIMIZE); ?> <?php disabled(!$imageEnvReady); ?>>
                                            <span class="yubw-choice__text">
                                                <span class="yubw-choice__label"><?php echo esc_html__('Compression', 'yub-wpanel-optimizer'); ?></span>
                                                <span class="yubw-choice__desc"><?php echo esc_html__('JPEG files are compressed at the selected quality, while PNG files are optimized losslessly. File formats and URLs remain unchanged.', 'yub-wpanel-optimizer'); ?></span>
                                            </span>
                                        </label>
                                        <label class="yubw-choice__item">
                                            <input id="yubw-image-mode-webp" type="radio" name="image_mode" value="webp" <?php checked($imageMode, self::IMAGE_MODE_WEBP); ?> <?php disabled(!$imageEnvReady); ?>>
                                            <span class="yubw-choice__text">
                                                <span class="yubw-choice__label"><?php echo esc_html__('WebP conversion', 'yub-wpanel-optimizer'); ?></span>
                                                <span class="yubw-choice__desc"><?php echo esc_html__('Convert to smaller WebP files and remove the original. Current versions of Chrome, Edge, Firefox, Safari, and other major browsers all display WebP.', 'yub-wpanel-optimizer'); ?></span>
                                            </span>
                                        </label>
                                    </div>
                                    <div class="yubw-input-group">
                                        <span class="yubw-input-group__item">
                                            <label for="yubw-image-jpeg-quality"><?php echo esc_html__('JPEG quality', 'yub-wpanel-optimizer'); ?></label>
                                            <input id="yubw-image-jpeg-quality" type="number" name="image_jpeg_quality" class="yubw-input yubw-input--short" value="<?php echo esc_attr($imageJpegQuality); ?>" min="1" max="100" <?php disabled(!$imageEnvReady); ?>>
                                        </span>
                                        <span class="yubw-input-group__item">
                                            <label for="yubw-image-webp-quality"><?php echo esc_html__('WebP quality', 'yub-wpanel-optimizer'); ?></label>
                                            <input id="yubw-image-webp-quality" type="number" name="image_webp_quality" class="yubw-input yubw-input--short" value="<?php echo esc_attr($imageWebpQuality); ?>" min="1" max="100" <?php disabled(!$imageEnvReady); ?>>
                                        </span>
                                    </div>
                                    <?php if ($imageMode === self::IMAGE_MODE_WEBP): ?>
                                        <p class="yubw-field__note"><?php echo sprintf(esc_html__('Modern browsers display WebP reliably. WebP mode does %1$snot keep the original%2$s, so test older plugins, email templates, social sharing, and external services that may require JPEG or PNG files.', 'yub-wpanel-optimizer'), '<strong>', '</strong>'); ?></p>
                                    <?php endif; ?>
                                    <?php if ($imageSkippedCount > 0): ?>
                                        <p class="yubw-field__note"><?php echo esc_html(sprintf(__('%d images could not be converted (unsupported file format, or the converted file would have been larger, etc.); the originals were kept automatically and uploads are unaffected.', 'yub-wpanel-optimizer'), $imageSkippedCount)); ?></p>
                                    <?php endif; ?>
                                </div>
                            </div>
                        </div>
                    </section>

                    <section class="yubw-section">
                        <header class="yubw-section__head">
                            <h2 class="yubw-section__title"><span class="dashicons dashicons-images-alt2" aria-hidden="true"></span><?php echo esc_html__('Optimize existing Media Library images', 'yub-wpanel-optimizer'); ?></h2>
                            <p class="yubw-section__desc"><?php echo esc_html__('Re-encode existing media library images in place to save space.', 'yub-wpanel-optimizer'); ?></p>
                        </header>
                        <div class="yubw-section__body">
                            <ul class="yubw-points" style="margin-top:16px">
                                <li><span class="yubw-points__label"><?php echo esc_html__('Scope', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('Only existing JPEG and PNG images in the Media Library are processed.', 'yub-wpanel-optimizer'); ?></li>
                                <li><span class="yubw-points__label"><?php echo esc_html__('Method', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('Lossless in-place re-encoding: file names stay the same, no WebP copies are created, and image references in published content are untouched.', 'yub-wpanel-optimizer'); ?></li>
                                <li><span class="yubw-points__label"><?php echo esc_html__('Execution', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('The task runs on YUB WPanel in the background; you can close this page once it starts.', 'yub-wpanel-optimizer'); ?></li>
                            </ul>
                            <div class="yubw-stats">
                                <div class="yubw-stats__item">
                                    <span class="yubw-stats__label"><?php echo esc_html__('Job status', 'yub-wpanel-optimizer'); ?></span>
                                    <span class="yubw-stats__value"><span id="yubw-image-batch-status" class="yubw-task-status"><?php echo esc_html__('Idle', 'yub-wpanel-optimizer'); ?></span></span>
                                </div>
                                <div class="yubw-stats__item">
                                    <span class="yubw-stats__label"><?php echo esc_html__('Total saved', 'yub-wpanel-optimizer'); ?></span>
                                    <span class="yubw-stats__value" id="yubw-image-batch-lifetime-value">—</span>
                                </div>
                            </div>
                            <div class="yubw-meter" id="yubw-image-batch-meter" style="display:none"><div class="yubw-meter__fill" id="yubw-image-batch-fill"></div></div>
                            <p class="yubw-field__note" id="yubw-image-batch-progress" style="display:none"></p>
                            <p class="yubw-field__note" id="yubw-image-batch-lifetime" style="display:none"></p>
                            <div class="yubw-ops">
                                <button type="button" id="yubw-image-batch-start" class="button button-primary"><?php echo esc_html__('Start batch optimization', 'yub-wpanel-optimizer'); ?></button>
                                <button type="button" id="yubw-image-batch-stop" class="button" style="display:none"><?php echo esc_html__('Stop', 'yub-wpanel-optimizer'); ?></button>
                                <p class="yubw-ops__note"><?php echo esc_html__('Speed is controlled by the panel; large libraries can take a while.', 'yub-wpanel-optimizer'); ?></p>
                            </div>
                        </div>
                    </section>
                </div>

                <div class="yubw-tab-panel" data-tab-panel="security" style="display:none">
                    <?php if ($fileLockEnabled): ?>
                        <p class="yubw-statusline is-warning"><span class="dashicons dashicons-warning" aria-hidden="true"></span><?php echo esc_html__('File protection is enabled. Settings that modify wp-config.php are read-only; other settings and actions remain available.', 'yub-wpanel-optimizer'); ?></p>
                    <?php endif; ?>
                    <section class="yubw-section">
                        <header class="yubw-section__head">
                            <h2 class="yubw-section__title"><span class="dashicons dashicons-lock" aria-hidden="true"></span><?php echo esc_html__('WordPress hardening', 'yub-wpanel-optimizer'); ?></h2>
                            <p class="yubw-section__desc"><?php echo esc_html__('Reduce unnecessary WordPress access points while keeping security settings synchronized with YUB WPanel.', 'yub-wpanel-optimizer'); ?></p>
                        </header>
                        <div class="yubw-section__body">
                            <div class="yubw-field">
                                <div class="yubw-field__head">
                                    <label class="yubw-field__title" for="yubw-no-updates"><?php echo esc_html__('Disable update checks', 'yub-wpanel-optimizer'); ?></label>
                                    <span class="yubw-field__summary"><?php echo esc_html__('Hides update notices in the WordPress dashboard. Designed for business sites that receive infrequent maintenance.', 'yub-wpanel-optimizer'); ?></span>
                                </div>
                                <div class="yubw-field__body">
                                    <label class="yubw-switch">
                                        <input id="yubw-no-updates" name="no_updates" type="checkbox" value="1" <?php checked($noUpdates); ?> <?php disabled($fileLockEnabled); ?>>
                                        <span class="yubw-switch__track"><span class="yubw-switch__thumb"></span></span>
                                        <span class="yubw-switch__text"><?php echo esc_html__('Block update checks for core, plugins, and themes', 'yub-wpanel-optimizer'); ?></span>
                                    </label>
                                    <ul class="yubw-points">
                                        <li><span class="yubw-points__label"><?php echo esc_html__('When enabled', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('WordPress no longer checks for or displays update notices for core, plugins, and themes.', 'yub-wpanel-optimizer'); ?></li>
                                        <li class="is-warn"><span class="yubw-points__label"><?php echo esc_html__('Security risk', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('Skipping updates for a long time leaves known vulnerabilities unpatched. Do not treat "no notices" as "no updates needed".', 'yub-wpanel-optimizer'); ?></li>
                                        <li><span class="yubw-points__label"><?php echo esc_html__('Recommendation', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('Turn this setting off periodically to check for updates, or manage updates from "WP Overview" in YUB WPanel. Re-enable the site file lock when maintenance is complete.', 'yub-wpanel-optimizer'); ?></li>
                                    </ul>
                                </div>
                            </div>
                            <div class="yubw-field">
                                <div class="yubw-field__head">
                                    <label class="yubw-field__title" for="yubw-no-file-edit"><?php echo esc_html__('Disable file editing', 'yub-wpanel-optimizer'); ?></label>
                                    <span class="yubw-field__summary"><?php echo esc_html__('Removes the dashboard code editors, reducing the chance of injected code.', 'yub-wpanel-optimizer'); ?></span>
                                </div>
                                <div class="yubw-field__body">
                                    <label class="yubw-switch">
                                        <input id="yubw-no-file-edit" name="no_file_edit" type="checkbox" value="1" <?php checked($noFileEdit); ?> <?php disabled($fileLockEnabled); ?>>
                                        <span class="yubw-switch__track"><span class="yubw-switch__thumb"></span></span>
                                        <span class="yubw-switch__text"><?php echo esc_html__('Prevent editing theme and plugin files in the dashboard', 'yub-wpanel-optimizer'); ?></span>
                                    </label>
                                    <p class="yubw-field__note"><?php echo esc_html__('Recommended. It only disables the built-in dashboard code editors; post editing and media uploads are not affected.', 'yub-wpanel-optimizer'); ?></p>
                                </div>
                            </div>
                            <div class="yubw-field">
                                <div class="yubw-field__head">
                                    <label class="yubw-field__title" for="yubw-wp-debug"><?php echo esc_html__('Enable debug mode', 'yub-wpanel-optimizer'); ?></label>
                                    <span class="yubw-field__summary"><?php echo esc_html__('Logs PHP errors to a file for troubleshooting.', 'yub-wpanel-optimizer'); ?></span>
                                </div>
                                <div class="yubw-field__body">
                                    <label class="yubw-switch">
                                        <input id="yubw-wp-debug" name="wp_debug" type="checkbox" value="1" <?php checked($wpDebug); ?> <?php disabled($fileLockEnabled); ?>>
                                        <span class="yubw-switch__track"><span class="yubw-switch__thumb"></span></span>
                                        <span class="yubw-switch__text"><?php echo esc_html__('Enable the debug log', 'yub-wpanel-optimizer'); ?></span>
                                    </label>
                                    <ul class="yubw-points">
                                        <li><span class="yubw-points__label"><?php echo esc_html__('Use', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('Diagnose white screens, 500 errors, or plugin faults.', 'yub-wpanel-optimizer'); ?></li>
                                        <li class="is-warn"><span class="yubw-points__label"><?php echo esc_html__('Note', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('Enable only temporarily while troubleshooting; turn it off afterwards.', 'yub-wpanel-optimizer'); ?></li>
                                    </ul>
                                    <details class="yubw-more">
                                        <summary><?php echo esc_html__('More details', 'yub-wpanel-optimizer'); ?></summary>
                                        <div class="yubw-more__body">
                                            <p><?php echo sprintf(esc_html__('Errors are written to %1$s and are not shown to visitors by default. To display errors on the page temporarily, configure it on the site details page in YUB WPanel.', 'yub-wpanel-optimizer'), '<code>wp-content/debug.log</code>'); ?></p>
                                        </div>
                                    </details>
                                </div>
                            </div>
                        </div>
                    </section>

                    <section class="yubw-section">
                        <header class="yubw-section__head">
                            <h2 class="yubw-section__title"><span class="dashicons dashicons-database" aria-hidden="true"></span><?php echo esc_html__('Resources & database', 'yub-wpanel-optimizer'); ?></h2>
                        </header>
                        <div class="yubw-section__body">
                            <div class="yubw-field">
                                <div class="yubw-field__head">
                                    <label class="yubw-field__title" for="yubw-post-revisions"><?php echo esc_html__('Post revisions', 'yub-wpanel-optimizer'); ?></label>
                                    <span class="yubw-field__summary"><?php echo esc_html__('Limit how many historical revisions each post keeps.', 'yub-wpanel-optimizer'); ?></span>
                                </div>
                                <div class="yubw-field__body">
                                    <input id="yubw-post-revisions" name="post_revisions" type="number" class="yubw-input yubw-input--num" value="<?php echo esc_attr($postRevisions >= 0 ? $postRevisions : ''); ?>" min="-1" placeholder="<?php echo esc_attr__('Default', 'yub-wpanel-optimizer'); ?>" <?php disabled($fileLockEnabled); ?>>
                                    <ul class="yubw-points">
                                        <li><span class="yubw-points__label"><?php echo esc_html__('Purpose', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('Keeps past versions of a post so accidental edits can be restored.', 'yub-wpanel-optimizer'); ?></li>
                                        <li><span class="yubw-points__label"><?php echo esc_html__('Recommendation', 'yub-wpanel-optimizer'); ?></span><?php echo sprintf(esc_html__('A value of %1$s3–5%2$s works for most sites; leave empty for no limit.', 'yub-wpanel-optimizer'), '<strong>', '</strong>'); ?></li>
                                        <li class="is-warn"><span class="yubw-points__label"><?php echo esc_html__('Entering 0', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('Disables revisions entirely; accidental edits can no longer be restored from revision history.', 'yub-wpanel-optimizer'); ?></li>
                                    </ul>
                                </div>
                            </div>
                            <div class="yubw-field">
                                <div class="yubw-field__head">
                                    <label class="yubw-field__title" for="yubw-memory-limit"><?php echo esc_html__('WordPress memory limit', 'yub-wpanel-optimizer'); ?></label>
                                    <span class="yubw-field__summary"><?php echo esc_html__('Adjust only when memory exhaustion errors appear.', 'yub-wpanel-optimizer'); ?></span>
                                </div>
                                <div class="yubw-field__body">
                                    <input id="yubw-memory-limit" name="memory_limit" type="text" class="yubw-input yubw-input--num" value="<?php echo esc_attr($memoryLimit); ?>" placeholder="<?php echo esc_attr__('Default 40M', 'yub-wpanel-optimizer'); ?>" <?php disabled($fileLockEnabled); ?>>
                                    <p class="yubw-field__note"><?php echo sprintf(esc_html__('Current PHP memory limit on this server: %1$s. If you are unsure, leave this blank. If more memory is needed, try %2$s or %3$s.', 'yub-wpanel-optimizer'), '<strong>' . esc_html($phpMemoryLimit) . '</strong>', '<strong>128M</strong>', '<strong>256M</strong>'); ?></p>
                                    <details class="yubw-more">
                                        <summary><?php echo esc_html__('More details', 'yub-wpanel-optimizer'); ?></summary>
                                        <div class="yubw-more__body">
                                            <p><?php echo esc_html__('Increase this if you see "Allowed memory size exhausted" errors or memory-related white screens in the dashboard.', 'yub-wpanel-optimizer'); ?></p>
                                            <p><?php echo esc_html__('The value here cannot exceed the PHP memory limit shown above, which comes from the effective configuration in "Software Management" in YUB WPanel.', 'yub-wpanel-optimizer'); ?></p>
                                        </div>
                                    </details>
                                </div>
                            </div>
                        </div>
                    </section>

                    <section class="yubw-section">
                        <header class="yubw-section__head">
                            <h2 class="yubw-section__title"><span class="dashicons dashicons-admin-network" aria-hidden="true"></span><?php echo esc_html__('Security features managed by YUB WPanel', 'yub-wpanel-optimizer'); ?></h2>
                            <p class="yubw-section__desc"><?php echo esc_html__('This plugin displays these settings as read-only. They can be changed only in YUB WPanel, so WordPress administrators cannot weaken server security policies.', 'yub-wpanel-optimizer'); ?></p>
                        </header>
                        <div class="yubw-section__body">
                            <div class="yubw-field">
                                <div class="yubw-field__head">
                                    <span class="yubw-field__title"><?php echo esc_html__('File protection & temporary maintenance', 'yub-wpanel-optimizer'); ?></span>
                                    <span class="yubw-field__summary"><?php echo esc_html__('Helps prevent accidental or malicious changes to plugins, themes, and other site code.', 'yub-wpanel-optimizer'); ?></span>
                                </div>
                                <div class="yubw-field__body">
                                    <div class="yubw-locked">
                                        <span class="yubw-locked__value <?php echo $fileLockEnabled ? 'is-ok' : 'is-warn'; ?>"><?php echo $fileLockEnabled ? esc_html__('File protection enabled', 'yub-wpanel-optimizer') : esc_html__('File protection not enabled', 'yub-wpanel-optimizer'); ?></span>
                                        <span class="yubw-locked__hint"><?php echo esc_html__('Configured in YUB WPanel → Site details → File protection & temporary maintenance', 'yub-wpanel-optimizer'); ?></span>
                                    </div>
                                    <ul class="yubw-points">
                                        <li><span class="yubw-points__label"><?php echo esc_html__('When enabled', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('Writing posts, editing pages, and uploading images are unaffected; installing, updating, or modifying program files is restricted.', 'yub-wpanel-optimizer'); ?></li>
                                        <li><span class="yubw-points__label"><?php echo esc_html__('During maintenance', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('Use the maintenance password configured in the panel to unlock temporarily, then relock the site as soon as maintenance is complete.', 'yub-wpanel-optimizer'); ?></li>
                                    </ul>
                                    <?php if ($showMaintenance): ?>
                                        <button type="button" class="button yubw-managed-action" data-yubw-maintenance-open><span class="dashicons dashicons-lock" aria-hidden="true"></span><?php echo esc_html__('View temporary maintenance', 'yub-wpanel-optimizer'); ?></button>
                                    <?php endif; ?>
                                </div>
                            </div>
                            <div class="yubw-field">
                                <div class="yubw-field__head">
                                    <span class="yubw-field__title"><?php echo esc_html__('XML-RPC interface', 'yub-wpanel-optimizer'); ?></span>
                                    <span class="yubw-field__summary"><?php echo esc_html__('A legacy remote connection API; most ordinary sites do not need it.', 'yub-wpanel-optimizer'); ?></span>
                                </div>
                                <div class="yubw-field__body">
                                    <div class="yubw-locked">
                                        <span class="yubw-locked__value <?php echo $xmlrpcEnabled ? 'is-warn' : 'is-ok'; ?>"><?php echo $xmlrpcEnabled ? esc_html__('Enabled', 'yub-wpanel-optimizer') : esc_html__('Disabled', 'yub-wpanel-optimizer'); ?></span>
                                        <span class="yubw-locked__hint"><?php echo esc_html__('Changed in YUB WPanel → Site details → WordPress optimization', 'yub-wpanel-optimizer'); ?></span>
                                    </div>
                                    <ul class="yubw-points">
                                        <li><span class="yubw-points__label"><?php echo esc_html__('Recommendation', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('Keep it off unless you use Jetpack, an older mobile app, or remote publishing tools.', 'yub-wpanel-optimizer'); ?></li>
                                        <li class="is-warn"><span class="yubw-points__label"><?php echo esc_html__('When to enable', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('Enable only when a tool you rely on clearly cannot connect.', 'yub-wpanel-optimizer'); ?></li>
                                    </ul>
                                </div>
                            </div>
                            <div class="yubw-field">
                                <div class="yubw-field__head">
                                    <span class="yubw-field__title"><?php echo esc_html__('WordPress application passwords', 'yub-wpanel-optimizer'); ?></span>
                                    <span class="yubw-field__summary"><?php echo esc_html__('Let mobile apps, auto-publishing tools, and third-party services connect to your site.', 'yub-wpanel-optimizer'); ?></span>
                                </div>
                                <div class="yubw-field__body">
                                    <div class="yubw-locked">
                                        <span class="yubw-locked__value <?php echo $applicationPasswordsDisabled ? 'is-ok' : 'is-idle'; ?>"><?php echo $applicationPasswordsDisabled ? esc_html__('Disabled', 'yub-wpanel-optimizer') : esc_html__('Allowed', 'yub-wpanel-optimizer'); ?></span>
                                        <span class="yubw-locked__hint"><?php echo esc_html__('Changed in YUB WPanel → Site details → WordPress optimization', 'yub-wpanel-optimizer'); ?></span>
                                    </div>
                                    <ul class="yubw-points">
                                        <li><span class="yubw-points__label"><?php echo esc_html__('Recommendation', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('Keep disabled on ordinary sites; enable when an external tool needs to connect.', 'yub-wpanel-optimizer'); ?></li>
                                        <li><span class="yubw-points__label"><?php echo esc_html__('Unaffected', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('Disabling does not affect dashboard login, writing posts, or uploading images.', 'yub-wpanel-optimizer'); ?></li>
                                        <li><span class="yubw-points__label"><?php echo esc_html__('Existing passwords', 'yub-wpanel-optimizer'); ?></span><?php echo esc_html__('Disabling does not delete existing credentials; they keep working once re-enabled.', 'yub-wpanel-optimizer'); ?></li>
                                    </ul>
                                </div>
                            </div>
                            <div class="yubw-field">
                                <div class="yubw-field__head">
                                    <span class="yubw-field__title"><?php echo esc_html__('WordPress anomaly monitoring', 'yub-wpanel-optimizer'); ?></span>
                                    <span class="yubw-field__summary"><?php echo esc_html__('Periodically checks for changes to administrators, content, security settings, and unusual persisted database objects.', 'yub-wpanel-optimizer'); ?></span>
                                </div>
                                <div class="yubw-field__body">
                                    <div class="yubw-locked">
                                        <span class="yubw-locked__value <?php echo $anomalyMonitorStatus === 'active' ? 'is-ok' : ($anomalyMonitorStatus === 'error' ? 'is-warn' : 'is-idle'); ?>"><?php echo esc_html($anomalyLabels[$anomalyMonitorStatus]); ?></span>
                                        <span class="yubw-locked__hint"><?php echo esc_html__('Configured and reviewed in YUB WPanel → Site details → WordPress anomaly monitoring', 'yub-wpanel-optimizer'); ?></span>
                                    </div>
                                    <p class="yubw-field__note"><?php echo esc_html__('Anomalies are recorded and notified by the panel; files, users, or content are never deleted automatically.', 'yub-wpanel-optimizer'); ?></p>
                                </div>
                            </div>
                            <div class="yubw-field">
                                <div class="yubw-field__head">
                                    <span class="yubw-field__title"><?php echo esc_html__('Password reset policy', 'yub-wpanel-optimizer'); ?></span>
                                    <span class="yubw-field__summary"><?php echo esc_html__('Controls which users may reset their login password via "Lost your password?".', 'yub-wpanel-optimizer'); ?></span>
                                </div>
                                <div class="yubw-field__body">
                                    <div class="yubw-locked">
                                        <span class="yubw-locked__value <?php echo $passwordResetMode === 'allow' ? 'is-idle' : 'is-ok'; ?>"><?php echo esc_html($passwordResetLabels[$passwordResetMode]); ?></span>
                                        <span class="yubw-locked__hint"><?php echo esc_html__('Changed in YUB WPanel → Site details → Password reset protection', 'yub-wpanel-optimizer'); ?></span>
                                    </div>
                                    <p class="yubw-field__note"><?php echo esc_html__('Restricting resets reduces the risk of malicious admin password resets; a forgotten password then requires another administrator or the panel owner to recover.', 'yub-wpanel-optimizer'); ?></p>
                                </div>
                            </div>
                        </div>
                    </section>
                </div>

                <div class="yubw-tab-panel" data-tab-panel="about" style="display:none">
                    <section class="yubw-section">
                        <header class="yubw-section__head">
                            <h2 class="yubw-section__title"><span class="dashicons dashicons-admin-links" aria-hidden="true"></span><?php echo esc_html__('About & panel sync', 'yub-wpanel-optimizer'); ?></h2>
                        </header>
                        <div class="yubw-section__body">
                            <p class="yubw-about-lead"><?php echo esc_html__('YUB WPanel Optimizer is the companion plugin installed automatically by YUB WPanel. It connects caching, image handling, and common WordPress settings to the panel for centralized management.', 'yub-wpanel-optimizer'); ?></p>
                            <div class="yubw-grid">
                                <div class="yubw-panel-card">
                                    <div class="yubw-panel-card__head">
                                        <span class="dashicons dashicons-admin-site-alt3" aria-hidden="true"></span>
                                        <h3><?php echo esc_html__('Current connection', 'yub-wpanel-optimizer'); ?></h3>
                                    </div>
                                    <div class="yubw-panel-card__body">
                                        <dl class="yubw-dl">
                                            <div class="yubw-dl__row">
                                                <dt><?php echo esc_html__('Site', 'yub-wpanel-optimizer'); ?></dt>
                                                <dd><?php echo esc_html($currentDomain); ?></dd>
                                            </div>
                                            <div class="yubw-dl__row">
                                                <dt><?php echo esc_html__('Plugin version', 'yub-wpanel-optimizer'); ?></dt>
                                                <dd><?php echo esc_html($pluginVersion); ?></dd>
                                            </div>
                                            <div class="yubw-dl__row">
                                                <dt>API Key</dt>
                                                <dd><?php echo esc_html($apiKey ? substr($apiKey, 0, 8) . '...' : __('Not set', 'yub-wpanel-optimizer')); ?></dd>
                                            </div>
                                            <div class="yubw-dl__row">
                                                <dt><?php echo esc_html__('Connection status', 'yub-wpanel-optimizer'); ?></dt>
                                                <dd class="<?php echo $missing ? 'is-risk' : 'is-ok'; ?>"><?php echo $missing ? esc_html__('Awaiting configuration', 'yub-wpanel-optimizer') : esc_html__('Configured', 'yub-wpanel-optimizer'); ?></dd>
                                            </div>
                                        </dl>
                                        <button type="button" id="yubw-verify-btn" class="button button-primary"><?php echo esc_html__('Verify panel connection', 'yub-wpanel-optimizer'); ?></button>
                                        <div id="yubw-verify-msg" aria-live="polite"></div>
                                    </div>
                                </div>
                                <div class="yubw-panel-card">
                                    <div class="yubw-panel-card__head">
                                        <span class="dashicons dashicons-lock" aria-hidden="true"></span>
                                        <h3><?php echo esc_html__('Credentials & updates', 'yub-wpanel-optimizer'); ?></h3>
                                    </div>
                                    <div class="yubw-panel-card__body">
                                        <p><?php echo esc_html__('The API key is the dedicated credential this plugin uses to connect to YUB WPanel; it is generated and stored by the panel automatically. Only the first 8 characters are shown here; the full key cannot be viewed or changed.', 'yub-wpanel-optimizer'); ?></p>
                                        <p><?php echo esc_html__('The plugin is installed and updated automatically by YUB WPanel; there is no need to download it separately from the WordPress dashboard.', 'yub-wpanel-optimizer'); ?></p>
                                        <p><?php echo esc_html__('"Verify panel connection" checks that the plugin can read and save settings properly; it is not a security scan of the site.', 'yub-wpanel-optimizer'); ?></p>
                                    </div>
                                </div>
                                <div class="yubw-panel-card">
                                    <div class="yubw-panel-card__head">
                                        <span class="dashicons dashicons-sos" aria-hidden="true"></span>
                                        <h3><?php echo esc_html__('Help & support', 'yub-wpanel-optimizer'); ?></h3>
                                    </div>
                                    <div class="yubw-panel-card__body">
                                        <p><?php echo esc_html__('Read the YUB WPanel documentation or report reproducible issues and error messages.', 'yub-wpanel-optimizer'); ?></p>
                                        <div class="yubw-links">
                                            <a class="button" href="https://github.com/zangwp/yub-wpanel" target="_blank" rel="noopener noreferrer"><span class="dashicons dashicons-external" aria-hidden="true"></span><?php echo esc_html__('Visit the YUB WPanel project', 'yub-wpanel-optimizer'); ?></a>
                                            <a class="button" href="https://github.com/zangwp/yub-wpanel/issues" target="_blank" rel="noopener noreferrer"><span class="dashicons dashicons-editor-help" aria-hidden="true"></span><?php echo esc_html__('Report a bug', 'yub-wpanel-optimizer'); ?></a>
                                        </div>
                                    </div>
                                </div>
                            </div>
                        </div>
                    </section>
                </div>

                <div class="yubw-actions">
                    <button type="submit" name="yubw_save" class="button button-primary"><?php echo esc_html__('Save settings', 'yub-wpanel-optimizer'); ?></button>
                    <?php if ($fileLockEnabled): ?>
                        <p class="yubw-actions__hint"><?php echo esc_html__('Available settings can still be saved. Read-only settings remain unchanged until file protection is temporarily unlocked.', 'yub-wpanel-optimizer'); ?></p>
                    <?php else: ?>
                        <p class="yubw-actions__hint"><?php echo esc_html__('Settings sync to YUB WPanel after saving; cache-related changes may take a few seconds to take effect.', 'yub-wpanel-optimizer'); ?></p>
                    <?php endif; ?>
                </div>
            </form>

            <!-- 这几个操作各自独立提交到 admin-post.php，不能嵌套在 yubw-form 里面
                 （HTML 不允许 form 嵌套 form，嵌套会导致浏览器解析时提前把外层
                 form 截断，图片优化等后面标签页的字段和保存按钮都会跟着掉出表单）。
                 用同一个 data-tab-panel="cache" 让标签页切换 JS 一并控制显隐。 -->
            <div class="yubw-tab-panel yubw-cache-status" data-tab-panel="cache">
                <section class="yubw-section">
                    <header class="yubw-section__head">
                        <h2 class="yubw-section__title"><span class="dashicons dashicons-dashboard" aria-hidden="true"></span><?php echo esc_html__('Cache actions & preload status', 'yub-wpanel-optimizer'); ?></h2>
                        <p class="yubw-section__desc"><?php echo esc_html__('Cache maintenance actions are separate from saving settings and take effect immediately.', 'yub-wpanel-optimizer'); ?></p>
                    </header>
                    <div class="yubw-section__body">
                        <p class="yubw-statusline <?php echo $preloadRunning ? 'is-active' : ''; ?>">
                            <span class="dashicons <?php echo $preloadRunning ? 'dashicons-update' : 'dashicons-yes-alt'; ?>" aria-hidden="true"></span>
                            <?php echo esc_html($preloadSummary); ?>
                        </p>
                        <div class="yubw-stats">
                            <div class="yubw-stats__item is-<?php echo esc_attr($stateTone); ?>">
                                <span class="yubw-stats__label"><?php echo esc_html__('Current status', 'yub-wpanel-optimizer'); ?></span>
                                <span class="yubw-stats__value"><?php echo esc_html($preloadRunning ? __('Running', 'yub-wpanel-optimizer') : __('Idle', 'yub-wpanel-optimizer')); ?></span>
                            </div>
                            <div class="yubw-stats__item is-<?php echo esc_attr($queueTone); ?>">
                                <span class="yubw-stats__label"><?php echo esc_html__('Queued', 'yub-wpanel-optimizer'); ?></span>
                                <span class="yubw-stats__value"><?php echo $preloadQueued; ?></span>
                            </div>
                            <div class="yubw-stats__item is-<?php echo esc_attr($doneTone); ?>">
                                <span class="yubw-stats__label"><?php echo esc_html__('Succeeded', 'yub-wpanel-optimizer'); ?></span>
                                <span class="yubw-stats__value"><?php echo $preloadDone; ?></span>
                            </div>
                            <div class="yubw-stats__item is-<?php echo esc_attr($failedTone); ?>">
                                <span class="yubw-stats__label"><?php echo esc_html__('Failed', 'yub-wpanel-optimizer'); ?></span>
                                <span class="yubw-stats__value"><?php echo $preloadFailed; ?></span>
                            </div>
                        </div>
                        <?php if (!empty($preloadStatus['last_message'])): ?>
                            <p class="yubw-field__note"><?php echo esc_html(sprintf(__('Last message: %s', 'yub-wpanel-optimizer'), $preloadStatus['last_message'])); ?></p>
                        <?php endif; ?>
                        <p class="yubw-field__note">
                            <?php if (!empty($preloadStatus['started_at'])): ?>
                                <?php echo esc_html__('Started:', 'yub-wpanel-optimizer'); ?> <?php echo esc_html($preloadStatus['started_at']); ?>　
                            <?php endif; ?>
                            <?php if (!empty($preloadStatus['last_run_at'])): ?>
                                <?php echo esc_html__('Last run:', 'yub-wpanel-optimizer'); ?> <?php echo esc_html($preloadStatus['last_run_at']); ?>　
                            <?php endif; ?>
                            <?php if (!empty($preloadStatus['finished_at'])): ?>
                                <?php echo esc_html__('Finished:', 'yub-wpanel-optimizer'); ?> <?php echo esc_html($preloadStatus['finished_at']); ?>
                            <?php endif; ?>
                        </p>

                        <div class="yubw-cache-actions">
                            <div class="yubw-cache-action">
                                <form method="post" action="<?php echo esc_url(admin_url('admin-post.php')); ?>">
                                    <?php wp_nonce_field('yubw_cache_clear'); ?>
                                    <input type="hidden" name="action" value="yubw_cache_clear">
                                    <button type="submit" class="button button-primary" <?php disabled($missing); ?>><?php echo esc_html__('Clear Nginx cache', 'yub-wpanel-optimizer'); ?></button>
                                </form>
                                <p><?php echo esc_html__('Use when content does not refresh promptly; if a CDN is in use, its cache must be cleared too.', 'yub-wpanel-optimizer'); ?></p>
                            </div>
                            <div class="yubw-cache-action">
                                <form method="post" action="<?php echo esc_url(admin_url('admin-post.php')); ?>">
                                    <?php wp_nonce_field('yubw_cache_preload'); ?>
                                    <input type="hidden" name="action" value="yubw_cache_preload">
                                    <button type="submit" class="button" <?php disabled(!$fcacheEnabled); ?>><?php echo esc_html__('Preload now', 'yub-wpanel-optimizer'); ?></button>
                                </form>
                                <p class="yubw-ops__reason <?php echo $fcacheEnabled ? '' : 'is-warn'; ?>" id="yubw-preload-reason"><?php echo esc_html($fcacheEnabled ? __('After saving settings, published pages will be preloaded according to the rules.', 'yub-wpanel-optimizer') : __('Preload unavailable: enable the FastCGI cache first and save.', 'yub-wpanel-optimizer')); ?></p>
                            </div>
                            <div class="yubw-cache-action">
                                <form method="post" action="<?php echo esc_url(admin_url('admin-post.php')); ?>">
                                    <?php wp_nonce_field('yubw_cache_preload_stop'); ?>
                                    <input type="hidden" name="action" value="yubw_cache_preload_stop">
                                    <button type="submit" class="button" <?php disabled(!$preloadStatus['running']); ?>><?php echo esc_html__('Stop preload', 'yub-wpanel-optimizer'); ?></button>
                                </form>
                                <p><?php echo esc_html__('You can stop a running task at any time. Pages that were already cached will remain cached.', 'yub-wpanel-optimizer'); ?></p>
                            </div>
                        </div>

                        <?php if (!empty($log)): ?>
                            <h3 class="yubw-subhead"><?php echo esc_html__('Recent cache clears', 'yub-wpanel-optimizer'); ?></h3>
                            <div class="yubw-log">
                                <table>
                                    <thead><tr><th><?php echo esc_html__('Time', 'yub-wpanel-optimizer'); ?></th><th><?php echo esc_html__('Trigger', 'yub-wpanel-optimizer'); ?></th><th><?php echo esc_html__('Result', 'yub-wpanel-optimizer'); ?></th></tr></thead>
                                    <tbody>
                                        <?php foreach ($log as $entry): ?>
                                        <tr>
                                            <td><?php echo esc_html($entry['time']); ?></td>
                                            <td><?php
                                                $labels = [
                                                    'manual'  => __('Manual clear', 'yub-wpanel-optimizer'),
                                                    'auto'    => __('Automatic (post published)', 'yub-wpanel-optimizer'),
                                                    'comment' => __('Automatic (comment activity)', 'yub-wpanel-optimizer'),
                                                ];
                                                echo esc_html($labels[$entry['type']] ?? __('Automatic', 'yub-wpanel-optimizer'));
                                            ?></td>
                                            <td><?php echo !empty($entry['success']) ? '<span class="is-ok">' . esc_html(__('Success', 'yub-wpanel-optimizer')) . '</span>' : '<span class="is-risk">' . esc_html(__('Failed', 'yub-wpanel-optimizer')) . '</span>'; ?></td>
                                        </tr>
                                        <?php endforeach; ?>
                                    </tbody>
                                </table>
                            </div>
                        <?php endif; ?>
                    </div>
                </section>
            </div>

            <script>
            // 由 PHP 传入的已翻译字符串（等效 wp_localize_script：本脚本内联在页面中，
            // 直接用 wp_json_encode 输出，避免在 JS 里硬编码用户可见文字）。
            var WPPSettingsL10n = <?php echo wp_json_encode([
                'preloadReasonOk'      => __('After saving settings, published pages will be preloaded according to the rules.', 'yub-wpanel-optimizer'),
                'preloadReasonBlocked' => __('Preload unavailable: enable the FastCGI cache first and save.', 'yub-wpanel-optimizer'),
                'verifying'            => __('Verifying…', 'yub-wpanel-optimizer'),
                'verifyOk'             => __('Connection verified. The YUB WPanel API authenticated the request and responded successfully.', 'yub-wpanel-optimizer'),
                'verifyFail'           => __('Connection failed: %s', 'yub-wpanel-optimizer'),
                'networkError'         => __('Network error: could not reach the panel (%s)', 'yub-wpanel-optimizer'),
                'unknownError'         => __('Unknown error', 'yub-wpanel-optimizer'),
                'updatingComponent'    => __('Updating…', 'yub-wpanel-optimizer'),
                'updateComplete'       => __('Update completed. Reloading…', 'yub-wpanel-optimizer'),
                'updateFailed'         => __('Update failed: %s', 'yub-wpanel-optimizer'),
                'statusQueued'         => __('Queued', 'yub-wpanel-optimizer'),
                'statusRunning'        => __('Running', 'yub-wpanel-optimizer'),
                'statusSucceeded'      => __('Completed', 'yub-wpanel-optimizer'),
                'statusFailed'         => __('Failed', 'yub-wpanel-optimizer'),
                'statusStopped'        => __('Stopped', 'yub-wpanel-optimizer'),
                'statusNone'           => __('Idle', 'yub-wpanel-optimizer'),
                'lifetimeSaved'        => __('%s MB saved in total (all batch jobs)', 'yub-wpanel-optimizer'),
                'progressBase'         => __('Progress: %s / %s', 'yub-wpanel-optimizer'),
                'progressFailed'       => __(' (%s failed)', 'yub-wpanel-optimizer'),
                'progressSkipped'      => __('; %s files were already processed earlier and were skipped this run', 'yub-wpanel-optimizer'),
                'progressSaved'        => __('; %s MB saved this run', 'yub-wpanel-optimizer'),
                'startFailed'          => __('Start failed: %s', 'yub-wpanel-optimizer'),
            ]); ?>;
            function wppFmt(str) {
                var args = Array.prototype.slice.call(arguments, 1);
                return String(str).replace(/%([sd%])/g, function(m, t) {
                    if (t === '%') return '%';
                    return args.length ? String(args.shift()) : m;
                });
            }
            // WordPress 的通知关闭按钮只隐藏元素，不会移除地址栏参数。清理本插件的
            // 一次性通知参数，避免用户刷新页面后再次看到已经关闭的提示。
            document.addEventListener('click', function(event) {
                var dismiss = event.target.closest('.notice-dismiss');
                if (!dismiss || !dismiss.closest('.yubw-settings')) return;
                var url = new URL(window.location.href);
                var changed = false;
                ['yubw_cleared', 'yubw_preload', 'count', '_wpnonce'].forEach(function(key) {
                    if (url.searchParams.has(key)) {
                        url.searchParams.delete(key);
                        changed = true;
                    }
                });
                if (changed) {
                    window.history.replaceState({}, '', url.pathname + url.search + url.hash);
                }
            });

            (function() {
                var tabs = document.querySelectorAll('#yubw-tabs .nav-tab');
                var panels = document.querySelectorAll('.yubw-tab-panel');
                function activate(name) {
                    tabs.forEach(function(t) { t.classList.toggle('nav-tab-active', t.dataset.tab === name); });
                    panels.forEach(function(p) { p.style.display = (p.dataset.tabPanel === name) ? '' : 'none'; });
                    try { sessionStorage.setItem('yubw_active_tab', name); } catch (e) {}
                }
                tabs.forEach(function(t) {
                    t.addEventListener('click', function(e) { e.preventDefault(); activate(t.dataset.tab); });
                });
                var saved = null;
                try { saved = sessionStorage.getItem('yubw_active_tab'); } catch (e) {}
                if (saved && document.querySelector('.yubw-tab-panel[data-tab-panel="' + saved + '"]')) {
                    activate(saved);
                }
            })();

            // 预加载依赖缓存：原因前置到开关旁，并直接绑定在按钮下方，
            // 不让用户自己去推断"为什么按钮是灰的"。
            (function() {
                var fcache = document.getElementById('yubw-fcache-enabled');
                var hint = document.getElementById('yubw-preload-requires-cache');
                var reason = document.getElementById('yubw-preload-reason');
                if (!fcache) return;
                function sync() {
                    var on = fcache.checked;
                    if (hint) hint.style.display = on ? 'none' : '';
                    if (reason) {
                        reason.textContent = on
                            ? WPPSettingsL10n.preloadReasonOk
                            : WPPSettingsL10n.preloadReasonBlocked;
                        reason.classList.toggle('is-warn', !on);
                    }
                }
                fcache.addEventListener('change', sync);
                sync();
            })();

            function wppNotice(el, type, textContent) {
                var div = document.createElement('div');
                div.className = 'yubw-page-notice yubw-page-notice--' + type;
                var p = document.createElement('p');
                p.textContent = textContent;
                div.appendChild(p);
                el.replaceChildren(div);
            }

            document.getElementById('yubw-verify-btn').addEventListener('click', function() {
                var btn = this, msg = document.getElementById('yubw-verify-msg');
                var label = btn.textContent;
                btn.disabled = true;
                btn.textContent = WPPSettingsL10n.verifying;
                fetch('<?php echo esc_url(admin_url('admin-ajax.php')); ?>?action=yubw_optimizer_verify&_wpnonce=<?php echo esc_attr(wp_create_nonce('yubw_optimizer_settings')); ?>')
                    .then(r => r.json())
                    .then(data => {
                        if (data.success) {
                            wppNotice(msg, 'success', WPPSettingsL10n.verifyOk);
                        } else {
                            wppNotice(msg, 'error', wppFmt(WPPSettingsL10n.verifyFail, data.data?.message || WPPSettingsL10n.unknownError));
                        }
                    })
                    .catch(e => {
                        wppNotice(msg, 'error', wppFmt(WPPSettingsL10n.networkError, e.message));
                    })
                    .finally(() => { btn.disabled = false; btn.textContent = label; });
            });

            var componentUpdateBtn = document.getElementById('yubw-component-update-btn');
            if (componentUpdateBtn) componentUpdateBtn.addEventListener('click', function() {
                var btn = this, msg = document.getElementById('yubw-component-update-msg');
                var label = btn.textContent;
                btn.disabled = true;
                btn.textContent = WPPSettingsL10n.updatingComponent;
                fetch('<?php echo esc_url(admin_url('admin-ajax.php')); ?>?action=yubw_optimizer_update_companion&_wpnonce=<?php echo esc_attr(wp_create_nonce('yubw_optimizer_settings')); ?>', { method: 'POST', credentials: 'same-origin' })
                    .then(r => r.json())
                    .then(data => {
                        if (!data.success) throw new Error(data.data?.message || WPPSettingsL10n.unknownError);
                        wppNotice(msg, 'success', WPPSettingsL10n.updateComplete);
                        window.setTimeout(() => window.location.reload(), 800);
                    })
                    .catch(e => {
                        wppNotice(msg, 'error', wppFmt(WPPSettingsL10n.updateFailed, e.message));
                        btn.disabled = false;
                        btn.textContent = label;
                    });
            });

            (function() {
                var startBtn = document.getElementById('yubw-image-batch-start');
                var stopBtn = document.getElementById('yubw-image-batch-stop');
                var statusEl = document.getElementById('yubw-image-batch-status');
                var progressEl = document.getElementById('yubw-image-batch-progress');
                var lifetimeEl = document.getElementById('yubw-image-batch-lifetime');
                var lifetimeValue = document.getElementById('yubw-image-batch-lifetime-value');
                var meterEl = document.getElementById('yubw-image-batch-meter');
                var fillEl = document.getElementById('yubw-image-batch-fill');
                var nonce = '<?php echo esc_attr(wp_create_nonce('yubw_optimizer_settings')); ?>';
                var ajaxUrl = '<?php echo esc_url(admin_url('admin-ajax.php')); ?>';
                var pollTimer = null;

                function call(action, extra) {
                    var params = new URLSearchParams(Object.assign({ action: action, _wpnonce: nonce }, extra || {}));
                    return fetch(ajaxUrl + '?' + params.toString(), { method: 'POST' }).then(r => r.json());
                }

                var statusLabels = {
                    queued: WPPSettingsL10n.statusQueued,
                    running: WPPSettingsL10n.statusRunning,
                    succeeded: WPPSettingsL10n.statusSucceeded,
                    failed: WPPSettingsL10n.statusFailed,
                    stopped: WPPSettingsL10n.statusStopped,
                    none: WPPSettingsL10n.statusNone
                };

                // LifetimeBytesSaved 是这个站点历史所有批量任务的累计节省量，跟当前
                // 有没有任务在跑无关——单次任务的节省量只统计"这次任务实际处理过的
                // 文件"，跳过的文件不计入，跳过越多，单次数字看起来就越小，容易被
                // 误以为"节省数据消失了"，所以单独展示一个不随任务重置的累计值。
                function renderLifetime(job) {
                    var lifetime = (job && job.LifetimeBytesSaved) || 0;
                    if (lifetime > 0) {
                        lifetimeEl.style.display = '';
                        lifetimeEl.textContent = wppFmt(WPPSettingsL10n.lifetimeSaved, (lifetime / 1024 / 1024).toFixed(2));
                        lifetimeValue.textContent = (lifetime / 1024 / 1024).toFixed(2) + ' MB';
                    } else {
                        lifetimeEl.style.display = 'none';
                        lifetimeValue.textContent = '—';
                    }
                }

                function renderMeter(job) {
                    var total = (job && job.TotalFiles) || 0;
                    var processed = (job && job.ProcessedFiles) || 0;
                    if (!total || total <= 0) {
                        meterEl.style.display = 'none';
                        fillEl.style.width = '0%';
                        return;
                    }
                    meterEl.style.display = '';
                    fillEl.style.width = Math.min(100, Math.max(0, (processed / total) * 100)).toFixed(1) + '%';
                }

                function render(job) {
                    renderLifetime(job);
                    renderMeter(job);
                    if (!job || !job.Status || job.Status === 'none') {
                        statusEl.textContent = WPPSettingsL10n.statusNone;
                        progressEl.style.display = 'none';
                        meterEl.style.display = 'none';
                        startBtn.style.display = '';
                        stopBtn.style.display = 'none';
                        return;
                    }
                    var running = job.Status === 'queued' || job.Status === 'running';
                    statusEl.textContent = statusLabels[job.Status] || job.Status;
                    startBtn.style.display = running ? 'none' : '';
                    stopBtn.style.display = running ? '' : 'none';
                    progressEl.style.display = '';
                    var text = wppFmt(WPPSettingsL10n.progressBase, job.ProcessedFiles || 0, job.TotalFiles || 0);
                    if (job.FailedFiles > 0) text += wppFmt(WPPSettingsL10n.progressFailed, job.FailedFiles);
                    if (job.SkippedFiles > 0) text += wppFmt(WPPSettingsL10n.progressSkipped, job.SkippedFiles);
                    var saved = (job.BytesBefore || 0) - (job.BytesAfter || 0);
                    if (saved > 0) text += wppFmt(WPPSettingsL10n.progressSaved, (saved / 1024 / 1024).toFixed(2));
                    progressEl.textContent = text;
                    if (running) {
                        clearTimeout(pollTimer);
                        pollTimer = setTimeout(poll, 3000);
                    }
                }

                function poll() {
                    call('yubw_optimizer_image_batch_status').then(function(data) {
                        if (data.success) render(data.data);
                    });
                }

                startBtn.addEventListener('click', function() {
                    startBtn.disabled = true;
                    call('yubw_optimizer_image_batch_start').then(function(data) {
                        startBtn.disabled = false;
                        if (data.success) {
                            render(data.data && data.data.Status ? data.data : { Status: 'queued' });
                            poll();
                        } else {
                            statusEl.textContent = wppFmt(WPPSettingsL10n.startFailed, data.data?.message || WPPSettingsL10n.unknownError);
                        }
                    });
                });

                stopBtn.addEventListener('click', function() {
                    stopBtn.disabled = true;
                    call('yubw_optimizer_image_batch_stop').then(function() {
                        stopBtn.disabled = false;
                        poll();
                    });
                });

                poll();
            })();
            </script>
        </div>
        <?php
    }

    public static function file_lock_notice() {
        if (!current_user_can('manage_options')) {
            return;
        }
        $screen = function_exists('get_current_screen') ? get_current_screen() : null;
        if ($screen && $screen->id === 'settings_page_yub-wpanel-optimizer') {
            return;
        }
        if (!self::sync_file_lock_state()) {
            return;
        }
        echo '<div class="notice notice-warning"><p><strong>' . esc_html__('YUB WPanel file lock is enabled.', 'yub-wpanel-optimizer') . '</strong> ' . esc_html__('Publishing posts, editing pages, and uploading media are unaffected. Protected plugin, theme, code, and site configuration files cannot be changed directly. If an installation, update, or setup task needs to write to these files, use File protection / maintenance in the upper-right corner to unlock the site temporarily with the maintenance password. If temporary maintenance is unavailable, ask the server administrator to handle the task in YUB WPanel. YUB WPanel Optimizer is managed by the panel and can still receive its own updates.', 'yub-wpanel-optimizer') . '</p></div>';
    }

}
