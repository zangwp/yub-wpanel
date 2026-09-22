<?php
/**
 * Plugin Name: YUB WPanel Inventory Adversarial Fixture
 * Version: 1.0.0
 */

if (!defined('YUB_WPANEL_INVENTORY_RUNNER')) {
    return;
}

$mode = defined('YUB_WPANEL_TEST_MODE') ? (string) YUB_WPANEL_TEST_MODE : 'normal';
switch ($mode) {
    case 'noise':
        echo str_repeat('fixture-noise', 32);
        break;
    case 'fatal':
        trigger_error('fixture fatal', E_USER_ERROR);
        break;
    case 'exit':
        exit(17);
    case 'timeout':
        sleep(30);
        break;
    case 'timeout_child':
        if (function_exists('pcntl_fork')) {
            $child = pcntl_fork();
            if ($child === 0) {
                sleep(30);
                exit(0);
            }
        }
        sleep(30);
        break;
    case 'memory':
        $chunks = array();
        while (true) {
            $chunks[] = str_repeat('M', 1024 * 1024);
        }
    case 'stdout_limit':
        while (ob_get_level() > 0) {
            ob_end_flush();
        }
        echo str_repeat('O', 70 * 1024);
        break;
    case 'stderr_limit':
        file_put_contents('php://stderr', str_repeat('E', 70 * 1024));
        break;
    case 'protocol_spoof':
        $fd = @fopen('php://fd/3', 'wb');
        if (is_resource($fd)) {
            $token = (string) getenv('YUB_WPANEL_RUNNER_TOKEN');
            fwrite($fd, 'YUB_WPANEL_INVENTORY_BEGIN ' . $token . "\n{}\nYUB_WPANEL_INVENTORY_END " . $token . "\n");
        }
        break;
    case 'security':
        $marker = WP_CONTENT_DIR . '/yub-wpanel-command-marker';
        @unlink($marker);
        $command = '/usr/bin/touch ' . escapeshellarg($marker);
        foreach (array('exec', 'passthru', 'shell_exec', 'system') as $function) {
            if (function_exists($function)) {
                try {
                    $function($command);
                } catch (Throwable $error) {
                }
            }
        }
        if (function_exists('popen')) {
            try {
                $handle = popen($command, 'r');
                if (is_resource($handle)) {
                    pclose($handle);
                }
            } catch (Throwable $error) {
            }
        }
        if (function_exists('proc_open')) {
            try {
                $process = proc_open($command, array(), $pipes);
                if (is_resource($process)) {
                    proc_close($process);
                }
            } catch (Throwable $error) {
            }
        }
        if (file_exists($marker)) {
            throw new RuntimeException('disabled command function executed');
        }
        if (@file_get_contents('/opt/yub-wpanel-runner-forbidden.txt') !== false) {
            throw new RuntimeException('open_basedir forbidden read succeeded');
        }
        add_filter('pre_http_request', static function () {
            throw new RuntimeException('runner triggered WordPress HTTP API');
        }, PHP_INT_MIN, 3);
        break;
}
