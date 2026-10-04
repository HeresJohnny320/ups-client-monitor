<?php
/*
 * register.php
 *
 * Adds (or removes) UPS Monitor's menu entry under Services and its entry on
 * Status > Services in pfSense's config. Run by "ups-monitor -install-pfsense"
 * and "-uninstall-pfsense" with pfSense's PHP:  php -f register.php install|uninstall
 */

require_once("config.inc");
require_once("util.inc");

$action = $argv[1] ?? 'install';
if ($action !== 'install' && $action !== 'uninstall') {
	fwrite(STDERR, "usage: php -f register.php install|uninstall\n");
	exit(1);
}

/* config_get_path/config_set_path exist on pfSense 2.7+; older versions use the global $config. */
function upsmon_cfg_get($path) {
	if (function_exists('config_get_path')) {
		$v = config_get_path($path, array());
		return is_array($v) ? $v : array();
	}
	global $config;
	list($a, $b) = explode('/', $path);
	return (isset($config[$a][$b]) && is_array($config[$a][$b])) ? $config[$a][$b] : array();
}

function upsmon_cfg_set($path, $value) {
	if (function_exists('config_set_path')) {
		config_set_path($path, $value);
		return;
	}
	global $config;
	list($a, $b) = explode('/', $path);
	if (!isset($config[$a]) || !is_array($config[$a])) {
		$config[$a] = array();
	}
	$config[$a][$b] = $value;
}

// Drop any earlier entries first, so running the installer again never adds duplicates.
$menus = array_values(array_filter(upsmon_cfg_get('installedpackages/menu'), function ($m) {
	return !(is_array($m) && ($m['name'] ?? '') === 'UPS Monitor');
}));
$services = array_values(array_filter(upsmon_cfg_get('installedpackages/service'), function ($s) {
	return !(is_array($s) && ($s['name'] ?? '') === 'ups_monitor');
}));

if ($action === 'install') {
	$menus[] = array(
		'name' => 'UPS Monitor',
		'tooltiptext' => 'Shut machines down on UPS battery and wake them afterwards',
		'section' => 'Services',
		'url' => '/ups_monitor.php',
	);
	$services[] = array(
		'name' => 'ups_monitor',
		'rcfile' => 'ups_monitor.sh',
		'executable' => 'ups-monitor',
		'description' => 'UPS Monitor (shutdown and Wake-on-LAN)',
	);
}

upsmon_cfg_set('installedpackages/menu', $menus);
upsmon_cfg_set('installedpackages/service', $services);
write_config($action === 'install' ? 'UPS Monitor: added menu entry and service' : 'UPS Monitor: removed menu entry and service');
echo "pfSense config updated ($action).\n";
