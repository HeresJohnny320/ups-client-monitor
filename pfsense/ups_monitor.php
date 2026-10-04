<?php
/*
 * ups_monitor.php
 *
 * UPS Monitor for pfSense: Services > UPS Monitor.
 * Installed by "ups-monitor -install-pfsense". Every change is sent to the running
 * monitor, which checks it, saves it and applies it straight away.
 */

require_once("guiconfig.inc");
require_once("/usr/local/pkg/ups_monitor.inc");

$tabs = array('status' => 'Status', 'plan' => 'Power Plan', 'machines' => 'Machines', 'settings' => 'Settings');
$tab = $_REQUEST['tab'] ?? 'status';
if (!isset($tabs[$tab])) {
	$tab = 'status';
}

/* ---------- Pieces of the Status tab (also served alone for the auto-refresh) ---------- */

function upsmon_down_box($r) {
	return '<div class="alert alert-danger"><p>' . upsmon_h($r['error'] ?? 'The UPS Monitor service is not running.') . '</p>' .
		'<form method="post" style="margin-top:10px"><input type="hidden" name="act" value="start">' .
		'<button type="submit" class="btn btn-success btn-sm">Start the service</button> ' .
		'<a href="status_services.php" class="btn btn-default btn-sm">Status &gt; Services</a></form></div>';
}

function upsmon_status_panels($st) {
	$out = '<!--upsmon-->';
	$out .= '<div class="panel panel-default"><div class="panel-heading"><h2 class="panel-title">Monitor</h2></div><div class="panel-body"><div class="content">';
	$out .= '<p>' . upsmon_connection_line($st) . '</p>';
	$out .= '<p class="text-muted" style="margin:0">Running as <b>' . upsmon_h($st['role'] ?: 'not set up') . '</b> · version ' .
		upsmon_h($st['version'] ?? '?') . ' · PID ' . (int)$st['pid'] . ' · started ' . upsmon_ago($st['started']) . '</p>';
	$out .= '</div></div></div>';

	$out .= '<div class="panel panel-default"><div class="panel-heading"><h2 class="panel-title">UPS</h2></div><div class="panel-body table-responsive">' .
		upsmon_ups_table($st) . '</div></div>';
	$out .= '<div class="panel panel-default"><div class="panel-heading"><h2 class="panel-title">Machines</h2></div><div class="panel-body table-responsive">' .
		upsmon_machine_table($st) . '</div></div>';

	$logs = array_slice($st['logs'] ?? array(), -150);
	$out .= '<div class="panel panel-default"><div class="panel-heading"><h2 class="panel-title">Recent activity</h2></div><div class="panel-body"><div class="content">' .
		'<pre id="upsmon-log" style="max-height:380px; overflow:auto; margin:0">' . upsmon_h(implode("\n", $logs)) . '</pre></div></div></div>';
	return $out;
}

if (($_GET['partial'] ?? '') === 'status') {
	$r = upsmon_call(array('cmd' => 'status'));
	if (!empty($r['ok'])) {
		echo upsmon_status_panels($r['status']);
	}
	exit;
}

/* ---------- Building settings from the forms ---------- */

function upsmon_default_server() {
	return array(
		'nut' => array('host' => '127.0.0.1', 'port' => 3493, 'username' => '', 'password' => ''),
		'poll_seconds' => 10, 'listen_port' => 3494, 'wake_after_restart' => true, 'webhook_url' => '',
		'machines' => array(),
		'self_tests' => array('enabled' => false, 'type' => 'quick', 'every_months' => 3, 'ups' => array(), 'webhook_url' => ''),
	);
}

function upsmon_default_client() {
	return array('server_address' => '', 'server_fingerprint' => '', 'name' => '', 'key' => '', 'failsafe_seconds' => 60, 'webhook_url' => '');
}

function upsmon_post($name) {
	return trim((string)($_POST[$name] ?? ''));
}

/* Secrets are never sent back to the browser: an empty field keeps the saved value. */
function upsmon_post_secret($name, $old) {
	$v = upsmon_post($name);
	return $v === '' ? (string)$old : $v;
}

function upsmon_machine_from_post($old) {
	$type = upsmon_post('type');
	$m = array(
		'name' => upsmon_post('name'),
		'type' => $type,
		'ups' => upsmon_post('ups'),
		'shutdown_at_percent' => (int)upsmon_post('shutdown_at'),
		'webhook_url' => upsmon_post('webhook'),
	);
	if ($type === 'proxmox') {
		$m['proxmox'] = array(
			'host' => upsmon_post('px_host'),
			'node' => upsmon_post('px_node'),
			'token' => upsmon_post_secret('px_token', $old['proxmox']['token'] ?? ''),
			'verify_tls' => !empty($_POST['px_verify']),
		);
	} elseif ($type === 'truenas') {
		$m['truenas'] = array(
			'host' => upsmon_post('tn_host'),
			'username' => upsmon_post('tn_user'),
			'api_key' => upsmon_post_secret('tn_key', $old['truenas']['api_key'] ?? ''),
			'verify_tls' => !empty($_POST['tn_verify']),
		);
	} elseif ($type === 'client') {
		$m['client_key'] = ($old['client_key'] ?? '') !== '' ? $old['client_key'] : upsmon_new_key();
	}
	if (upsmon_post('wake_mac') !== '' || $type === 'wake_only') {
		$m['wake'] = array(
			'mac' => upsmon_post('wake_mac'),
			'at_percent' => (int)upsmon_post('wake_at'),
			'broadcast' => upsmon_post('wake_bcast'),
		);
	}
	return $m;
}

/* ---------- Handle the form that was posted ---------- */

$input_errors = array();
$savemsg = '';
$pairing = null;      // pairing codes to show: array('name' => ..., 'codes' => [...])
$edit = null;         // machine shown in the edit form
$edit_idx = null;     // its index, -1 for a new one

if (($_POST['act'] ?? '') === 'start') {
	mwexec(UPSMON_RC . ' start');
	sleep(2);
}

$resp = upsmon_call(array('cmd' => 'get_settings'));
$down = empty($resp['ok']);
$s = $down ? array() : ($resp['settings'] ?? array());
$machines = $s['server']['machines'] ?? array();

function upsmon_save(&$s, $what) {
	global $input_errors, $savemsg;
	$r = upsmon_call(array('cmd' => 'set_settings', 'settings' => $s));
	if (empty($r['ok'])) {
		$input_errors[] = $r['error'];
		return false;
	}
	$s = $r['settings'];
	$savemsg = $what . ' saved and applied.';
	return true;
}

function upsmon_show_pairing($name) {
	global $pairing, $input_errors;
	$r = upsmon_call(array('cmd' => 'pairing', 'name' => $name));
	if (empty($r['ok'])) {
		$input_errors[] = $r['error'];
		return;
	}
	$pairing = array('name' => $name, 'codes' => $r['pairing']);
}

if ($_POST && !$down) {
	$act = $_POST['act'] ?? '';
	$idx = isset($_POST['idx']) ? (int)$_POST['idx'] : -1;
	$old = ($idx >= 0 && isset($machines[$idx])) ? $machines[$idx] : array();

	switch ($act) {
	case 'save_machine':
		$m = upsmon_machine_from_post($old);
		$new = $s;
		if ($idx >= 0) {
			$new['server']['machines'][$idx] = $m;
		} else {
			$new['server']['machines'][] = $m;
		}
		if (upsmon_save($new, $m['name'])) {
			$s = $new;
			if ($m['type'] === 'client') {
				upsmon_show_pairing($m['name']);
			}
		} else {
			list($edit, $edit_idx) = array($m, $idx);
		}
		break;

	case 'test_machine':
		$edit = upsmon_machine_from_post($old);
		$edit_idx = $idx;
		$r = upsmon_call(array('cmd' => 'test_machine', 'machine' => $edit));
		if (empty($r['ok'])) {
			$input_errors[] = 'Connection test failed: ' . $r['error'];
		} else {
			$savemsg = 'Connection test passed: ' . $r['info'] . '. Nothing was shut down.';
		}
		break;

	case 'wake_machine':
		$edit = upsmon_machine_from_post($old);
		$edit_idx = $idx;
		$r = upsmon_call(array('cmd' => 'wake', 'machine' => $edit));
		if (empty($r['ok'])) {
			$input_errors[] = 'Wake-on-LAN failed: ' . $r['error'];
		} else {
			$savemsg = $r['info'] . '.';
		}
		break;

	case 'delete_machine':
		if (isset($machines[$idx])) {
			$new = $s;
			array_splice($new['server']['machines'], $idx, 1);
			if (upsmon_save($new, 'Removed ' . $machines[$idx]['name'] . ';')) {
				$s = $new;
			}
		}
		break;

	case 'pairing':
		upsmon_show_pairing(upsmon_post('name'));
		break;

	case 'new_key':
		if (isset($machines[$idx]) && $machines[$idx]['type'] === 'client') {
			$new = $s;
			$new['server']['machines'][$idx]['client_key'] = upsmon_new_key();
			if (upsmon_save($new, 'New key for ' . $machines[$idx]['name'] . ';')) {
				$s = $new;
				upsmon_show_pairing($machines[$idx]['name']);
			}
		}
		break;

	case 'save_settings':
		$new = $s;
		$shown = upsmon_post('shown_role');
		if ($shown === 'server') {
			$srv = $new['server'] ?? upsmon_default_server();
			$srv['nut']['host'] = upsmon_post('nut_host');
			$srv['nut']['port'] = (int)upsmon_post('nut_port');
			$srv['nut']['username'] = upsmon_post('nut_user');
			$srv['nut']['password'] = upsmon_post_secret('nut_pass', $srv['nut']['password'] ?? '');
			$srv['poll_seconds'] = (int)upsmon_post('poll');
			$srv['listen_port'] = (int)upsmon_post('listen_port');
			$srv['wake_after_restart'] = !empty($_POST['wake_after_restart']);
			$srv['webhook_url'] = upsmon_post('webhook');
			$srv['self_tests']['enabled'] = !empty($_POST['st_enabled']);
			$srv['self_tests']['type'] = upsmon_post('st_type');
			$srv['self_tests']['every_months'] = (int)upsmon_post('st_months');
			$srv['self_tests']['ups'] = array_values(array_filter(array_map('trim', explode(',', upsmon_post('st_ups')))));
			$srv['self_tests']['webhook_url'] = upsmon_post('st_webhook');
			$srv['machines'] = $srv['machines'] ?? array();
			$new['server'] = $srv;
		} elseif ($shown === 'client') {
			$c = $new['client'] ?? upsmon_default_client();
			$code = upsmon_post('pairing_code');
			if ($code !== '') {
				$p = upsmon_parse_pairing($code);
				if ($p === null) {
					$input_errors[] = "That doesn't look like a pairing code. It should start with upsmon:// (copy it again from the server).";
					break;
				}
				$c = array_merge($c, $p);
			} else {
				$c['server_address'] = upsmon_post('server_address');
				$c['name'] = upsmon_post('client_name');
				$c['key'] = upsmon_post_secret('client_key', $c['key'] ?? '');
				$c['server_fingerprint'] = upsmon_post('server_fingerprint');
			}
			$c['failsafe_seconds'] = (int)upsmon_post('failsafe');
			$c['webhook_url'] = upsmon_post('webhook');
			$new['client'] = $c;
		}
		$new['role'] = upsmon_post('role');
		if (upsmon_save($new, 'Settings')) {
			$s = $new;
		}
		break;

	case 'action':
		$names = array('test' => 'Quick self-test', 'test-long' => 'Deep self-test', 'check-targets' => 'Login check', 'test-webhook' => 'Webhook test');
		$a = upsmon_post('action');
		$r = upsmon_call(array('cmd' => 'action', 'action' => $a));
		if (empty($r['ok'])) {
			$input_errors[] = $r['error'];
		} else {
			$savemsg = ($names[$a] ?? $a) . ' started. The results show up under Recent activity.';
		}
		break;
	}
	$machines = $s['server']['machines'] ?? array();
}

if ($edit === null && $tab === 'machines' && isset($_GET['edit']) && !$down) {
	$edit_idx = $_GET['edit'] === 'new' ? -1 : (int)$_GET['edit'];
	if ($edit_idx >= 0 && isset($machines[$edit_idx])) {
		$edit = $machines[$edit_idx];
	} else {
		$edit_idx = -1;
		$edit = array('type' => 'proxmox', 'shutdown_at_percent' => 50, 'ups' => '');
	}
}

$status = $down ? null : upsmon_call(array('cmd' => 'status'));
$st = (!empty($status['ok'])) ? $status['status'] : array();
$role = $s['role'] ?? '';

/* UPS names for the suggestions in the UPS fields. */
$ups_names = array();
foreach ($st['ups'] ?? array() as $u) {
	$ups_names[$u['name']] = true;
}
foreach ($machines as $m) {
	if (($m['ups'] ?? '') !== '') {
		$ups_names[$m['ups']] = true;
	}
}
$ups_names = array_keys($ups_names);
sort($ups_names);

/* ---------- Small form helpers (pfSense / Bootstrap 3 markup) ---------- */

function upsmon_row($label, $field, $help = '', $id = '') {
	echo '<div class="form-group"' . ($id ? ' id="' . $id . '"' : '') . '><label class="col-sm-2 control-label">' . upsmon_h($label) . '</label>' .
		'<div class="col-sm-10">' . $field . ($help !== '' ? '<span class="help-block">' . $help . '</span>' : '') . '</div></div>';
}

function upsmon_input($name, $value, $type = 'text', $extra = '') {
	return '<input class="form-control" type="' . $type . '" name="' . $name . '" value="' . upsmon_h($value) . '" ' . $extra . '>';
}

function upsmon_secret($name, $saved) {
	$ph = $saved !== '' ? 'saved (leave empty to keep it)' : '';
	return '<input class="form-control" type="password" name="' . $name . '" value="" placeholder="' . upsmon_h($ph) . '" autocomplete="new-password">';
}

function upsmon_select($name, $options, $selected, $extra = '') {
	$out = '<select class="form-control" name="' . $name . '" ' . $extra . '>';
	foreach ($options as $value => $label) {
		$out .= '<option value="' . upsmon_h($value) . '"' . ((string)$value === (string)$selected ? ' selected' : '') . '>' . upsmon_h($label) . '</option>';
	}
	return $out . '</select>';
}

function upsmon_check($name, $checked, $text) {
	return '<label class="chkboxlbl"><input type="checkbox" name="' . $name . '" value="1"' . ($checked ? ' checked' : '') . '> ' . upsmon_h($text) . '</label>';
}

function upsmon_panel_open($title) {
	echo '<div class="panel panel-default"><div class="panel-heading"><h2 class="panel-title">' . upsmon_h($title) . '</h2></div><div class="panel-body">';
}

function upsmon_panel_close() {
	echo '</div></div>';
}

/* A panel for text and tables-with-text, padded the way pfSense pages pad them. */
function upsmon_box_open($title) {
	upsmon_panel_open($title);
	echo '<div class="content">';
}

function upsmon_box_close() {
	echo '</div>';
	upsmon_panel_close();
}

/* ---------- Page ---------- */

$pgtitle = array(gettext("Services"), "UPS Monitor", $tabs[$tab]);
$pglinks = array("", "ups_monitor.php", "@self");
include("head.inc");

if ($input_errors) {
	print_input_errors($input_errors);
}
if ($savemsg) {
	print_info_box(upsmon_h($savemsg), 'success');
}

$tab_array = array();
foreach ($tabs as $key => $label) {
	$tab_array[] = array($label, $tab === $key, "ups_monitor.php?tab=" . $key);
}
display_top_tabs($tab_array);

if ($down) {
	echo upsmon_down_box($resp);
	include("foot.inc");
	exit;
}

if ($role === '' && $tab !== 'settings') {
	print_info_box('UPS Monitor isn\'t set up yet. Start on the <a href="ups_monitor.php?tab=settings">Settings</a> tab: ' .
		'choose <b>Server</b> if this firewall should watch the UPS and shut machines down.', 'info');
}

if ($pairing !== null) {
	upsmon_box_open('Pairing code for ' . $pairing['name']);
	echo '<p>On that computer, run <code>ups-monitor</code>, choose <b>Client</b>, open <b>Server Connection</b> and paste this code. ' .
		'Treat it like a password. Use the address the client can reach (normally your LAN address).</p>';
	foreach ($pairing['codes'] as $i => $p) {
		echo '<div class="form-group"><label>' . upsmon_h($p['address']) . '</label><div class="input-group">' .
			'<input class="form-control" type="text" readonly id="upsmon-code-' . $i . '" value="' . upsmon_h($p['code']) . '" onclick="this.select()">' .
			'<span class="input-group-btn"><button class="btn btn-default" type="button" onclick="upsmonCopy(' . $i . ', this)">Copy</button></span></div></div>';
	}
	echo '<p class="text-muted" style="margin:0">Clients connect to TCP port ' . (int)($s['server']['listen_port'] ?? 3494) .
		' on this firewall. The default LAN rule allows that; for other interfaces add a pass rule. Don\'t open it on WAN.</p>';
	upsmon_box_close();
}

/* ----- Status ----- */
if ($tab === 'status') {
	echo '<div id="upsmon-live">' . upsmon_status_panels($st) . '</div>';

	upsmon_box_open('Actions');
	echo '<form method="post" class="form-inline"><input type="hidden" name="tab" value="status"><input type="hidden" name="act" value="action">';
	if ($role === 'server') {
		echo '<button class="btn btn-default btn-sm" name="action" value="test">Quick self-test now</button> ';
		echo '<button class="btn btn-default btn-sm" name="action" value="test-long">Deep self-test now</button> ';
		echo '<button class="btn btn-default btn-sm" name="action" value="check-targets">Check Proxmox / TrueNAS logins</button> ';
	}
	echo '<button class="btn btn-default btn-sm" name="action" value="test-webhook">Send test webhooks</button>';
	echo '</form>';
	upsmon_box_close();
	?>
<script>
(function () {
	var box = document.getElementById('upsmon-live');
	function toBottom() {
		var log = document.getElementById('upsmon-log');
		if (log) { log.scrollTop = log.scrollHeight; }
	}
	toBottom();
	setInterval(function () {
		var log = document.getElementById('upsmon-log');
		var atBottom = !log || log.scrollTop + log.clientHeight >= log.scrollHeight - 20;
		fetch('ups_monitor.php?partial=status', {credentials: 'same-origin'})
			.then(function (r) { return r.ok ? r.text() : ''; })
			.then(function (html) {
				// Only replace with our own output (not e.g. a login page after a timeout).
				if (html.indexOf('<!--upsmon-->') !== 0) { return; }
				box.innerHTML = html;
				if (atBottom) { toBottom(); }
			});
	}, 5000);
})();
</script>
	<?php
}

/* ----- Power plan ----- */
if ($tab === 'plan') {
	if ($role !== 'server') {
		print_info_box('The power plan belongs to the server. This firewall is set up as a ' . upsmon_h($role ?: 'nothing yet') . '.', 'info');
	} else {
		$live = array();
		foreach ($st['ups'] ?? array() as $u) {
			$live[$u['name']] = $u;
		}
		if (empty($ups_names)) {
			print_info_box('No UPS seen yet and no machines added. Check the NUT settings, then add machines.', 'info');
		}
		foreach ($ups_names as $ups) {
			$state = isset($live[$ups]) ? upsmon_power_label($live[$ups]['status']) . ' ' .
				(!empty($live[$ups]['charge_known']) ? (int)$live[$ups]['charge'] . '%' : '') : '<span class="text-muted">not seen on NUT yet</span>';
			$down_steps = array();
			$up_steps = array();
			$no_wake = array();
			foreach ($machines as $i => $m) {
				if ($m['ups'] !== $ups) {
					continue;
				}
				$link = '<a href="ups_monitor.php?tab=machines&amp;edit=' . $i . '">' . upsmon_h($m['name']) . '</a>';
				if ($m['type'] !== 'wake_only') {
					$down_steps[] = array((int)$m['shutdown_at_percent'], $link, $upsmon_types[$m['type']]);
					if (empty($m['wake'])) {
						$no_wake[] = $link;
					}
				}
				if (!empty($m['wake'])) {
					$up_steps[] = array((int)$m['wake']['at_percent'], $link, upsmon_h($m['wake']['mac']));
				}
			}
			usort($down_steps, function ($a, $b) { return $b[0] - $a[0]; });
			usort($up_steps, function ($a, $b) { return $a[0] - $b[0]; });

			upsmon_box_open('UPS ' . $ups);
			echo '<p>' . $state . '</p><div class="row"><div class="col-sm-6"><h4>On battery, as the charge drops</h4>';
			echo '<table class="table table-condensed">';
			foreach ($down_steps as $d) {
				echo '<tr><td style="width:90px">≤ ' . $d[0] . '%</td><td>shut down ' . $d[1] . '</td><td class="text-muted">' . upsmon_h($d[2]) . '</td></tr>';
			}
			echo $down_steps ? '' : '<tr><td class="text-muted">Nothing is shut down.</td></tr>';
			echo '</table></div><div class="col-sm-6"><h4>Power back, as the battery recharges</h4><table class="table table-condensed">';
			foreach ($up_steps as $w) {
				echo '<tr><td style="width:90px">≥ ' . $w[0] . '%</td><td>wake ' . $w[1] . '</td><td class="text-muted">' . $w[2] . '</td></tr>';
			}
			echo $up_steps ? '' : '<tr><td class="text-muted">Nothing is woken.</td></tr>';
			echo '</table></div></div>';
			foreach ($no_wake as $n) {
				echo '<p class="text-warning">' . $n . ' has no Wake-on-LAN MAC, so it stays off after an outage.</p>';
			}
			upsmon_box_close();
		}
		echo '<p>Machines only wake after a power event (their UPS went on battery, or the monitor restarted). ' .
			'Once the UPS is back on mains and at the wake level, three packets are sent five minutes apart.</p>';
	}
}

/* ----- Machines ----- */
if ($tab === 'machines') {
	if ($role !== 'server') {
		print_info_box('Machines are managed by the server. This firewall is set up as a ' . upsmon_h($role ?: 'nothing yet') . '.', 'info');
	} elseif ($edit === null) {
		$live = array();
		foreach ($st['machines'] ?? array() as $lm) {
			$live[$lm['name']] = $lm;
		}
		upsmon_box_open('Machines');
		echo '<p>Everything the server shuts down and/or wakes: Proxmox, TrueNAS, PCs running this app, and Wake-on-LAN-only devices.</p>';
		echo '<div class="table-responsive"><table class="table table-striped table-hover table-condensed"><thead><tr>' .
			'<th>Name</th><th>Type</th><th>UPS</th><th>Off at</th><th>On at</th><th>State</th><th></th></tr></thead><tbody>';
		foreach ($machines as $i => $m) {
			$off = $m['type'] === 'wake_only' ? '—' : '≤ ' . (int)$m['shutdown_at_percent'] . '%';
			$on = empty($m['wake']) ? '—' : '≥ ' . (int)$m['wake']['at_percent'] . '%';
			echo '<tr><td>' . upsmon_h($m['name']) . '</td><td>' . upsmon_h($upsmon_types[$m['type']] ?? $m['type']) . '</td><td>' .
				upsmon_h($m['ups']) . '</td><td>' . $off . '</td><td>' . $on . '</td><td>' . upsmon_h($live[$m['name']]['shutdown_state'] ?? '') . '</td>' .
				'<td style="white-space:nowrap"><a class="btn btn-xs btn-info" href="ups_monitor.php?tab=machines&amp;edit=' . $i . '">Edit</a> ';
			if ($m['type'] === 'client') {
				echo '<form method="post" style="display:inline"><input type="hidden" name="tab" value="machines"><input type="hidden" name="act" value="pairing">' .
					'<input type="hidden" name="name" value="' . upsmon_h($m['name']) . '"><button class="btn btn-xs btn-default">Pairing code</button></form> ';
			}
			echo '<form method="post" style="display:inline" onsubmit="return confirm(\'Remove ' . upsmon_h(addslashes($m['name'])) . '? It will no longer be shut down or woken.\')">' .
				'<input type="hidden" name="tab" value="machines"><input type="hidden" name="act" value="delete_machine"><input type="hidden" name="idx" value="' . $i . '">' .
				'<button class="btn btn-xs btn-danger">Delete</button></form></td></tr>';
		}
		if (!$machines) {
			echo '<tr><td colspan="7" class="text-muted">No machines yet.</td></tr>';
		}
		echo '</tbody></table></div>';
		echo '<a class="btn btn-success btn-sm" href="ups_monitor.php?tab=machines&amp;edit=new">Add machine</a>';
		upsmon_box_close();
	} else {
		$m = $edit;
		$px = $m['proxmox'] ?? array();
		$tn = $m['truenas'] ?? array();
		$wk = $m['wake'] ?? array('at_percent' => 80);
		echo '<datalist id="upsmon-ups">';
		foreach ($ups_names as $n) {
			echo '<option value="' . upsmon_h($n) . '">';
		}
		echo '</datalist>';

		echo '<form method="post" class="form-horizontal"><input type="hidden" name="tab" value="machines"><input type="hidden" name="idx" value="' . (int)$edit_idx . '">';
		upsmon_panel_open($edit_idx >= 0 ? 'Edit ' . ($m['name'] ?? '') : 'Add machine');
		upsmon_row('Type', upsmon_select('type', $upsmon_types, $m['type'] ?? 'proxmox', 'id="upsmon-type" onchange="upsmonType()"'));
		upsmon_row('Name', upsmon_input('name', $m['name'] ?? ''), 'Any name you like. A client uses it to identify itself, so renaming a client means pairing it again.');
		upsmon_row('UPS', upsmon_input('ups', $m['ups'] ?? '', 'text', 'list="upsmon-ups"'),
			'The UPS that powers it' . ($ups_names ? ' (' . upsmon_h(implode(', ', $ups_names)) . ')' : '') . '.');
		upsmon_row('Shut down at', upsmon_input('shutdown_at', $m['shutdown_at_percent'] ?? 50, 'number', 'min="0" max="100"'),
			'Battery % at which it is shut down while on battery. NUT\'s low-battery signal also triggers it.', 'upsmon-shutdown');

		echo '<div class="upsmon-type upsmon-type-proxmox">';
		upsmon_row('Host', upsmon_input('px_host', $px['host'] ?? '', 'text', 'placeholder="https://192.168.1.10:8006"'));
		upsmon_row('Node name', upsmon_input('px_node', $px['node'] ?? ''), 'As shown in the Proxmox sidebar.');
		upsmon_row('API token', upsmon_secret('px_token', $px['token'] ?? ''), 'USER@REALM!TOKENID=SECRET, with Sys.PowerMgmt on /nodes/&lt;node&gt;.');
		upsmon_row('', upsmon_check('px_verify', !empty($px['verify_tls']), 'Verify TLS certificate (leave off for the default self-signed one)'));
		echo '</div><div class="upsmon-type upsmon-type-truenas">';
		upsmon_row('Host', upsmon_input('tn_host', $tn['host'] ?? '', 'text', 'placeholder="https://192.168.1.20"'),
			'Must be https:// (TrueNAS revokes API keys sent over plain HTTP).');
		upsmon_row('Username', upsmon_input('tn_user', $tn['username'] ?? ''), 'The user that owns the API key, e.g. truenas_admin.');
		upsmon_row('API key', upsmon_secret('tn_key', $tn['api_key'] ?? ''), 'Settings (top-right) → API Keys → Add.');
		upsmon_row('', upsmon_check('tn_verify', !empty($tn['verify_tls']), 'Verify TLS certificate (leave off for the default self-signed one)'));
		echo '</div><div class="upsmon-type upsmon-type-client">';
		upsmon_row('', '<p class="form-control-static">A computer running this app in client mode. After saving you get a pairing code to paste into it.</p>');
		echo '</div>';
		upsmon_panel_close();

		upsmon_panel_open('Turn it back on');
		upsmon_row('Wake-on-LAN MAC', upsmon_input('wake_mac', $wk['mac'] ?? '', 'text', 'placeholder="aa:bb:cc:dd:ee:ff"'),
			'Leave empty if it shouldn\'t be woken. Enable Wake-on-LAN in its BIOS and network adapter.');
		upsmon_row('Wake at', upsmon_input('wake_at', $wk['at_percent'] ?? 80, 'number', 'min="0" max="100"'),
			'After an outage, wake it once the UPS is back on mains and charged to this %.');
		upsmon_row('Broadcast address', upsmon_input('wake_bcast', $wk['broadcast'] ?? '', 'text', 'placeholder="255.255.255.255"'),
			'Only needed for another subnet, e.g. 192.168.20.255.');
		upsmon_row('Discord webhook', upsmon_input('webhook', $m['webhook_url'] ?? ''), 'Optional. Leave empty to use the one under Settings.');
		upsmon_panel_close();

		echo '<button class="btn btn-primary" name="act" value="save_machine">Save</button> ';
		echo '<button class="btn btn-default upsmon-type upsmon-type-proxmox upsmon-type-truenas" name="act" value="test_machine">Test connection</button> ';
		echo '<button class="btn btn-default" name="act" value="wake_machine">Wake now</button> ';
		if ($edit_idx >= 0 && ($m['type'] ?? '') === 'client') {
			echo '<button class="btn btn-default" name="act" value="new_key" onclick="return confirm(\'Give it a new key? It disconnects now and has to be paired again.\')">New key</button> ';
		}
		echo '<a class="btn btn-link" href="ups_monitor.php?tab=machines">Cancel</a>';
		echo '</form>';
		?>
<script>
function upsmonType() {
	var t = document.getElementById('upsmon-type').value;
	document.querySelectorAll('.upsmon-type').forEach(function (el) {
		el.style.display = el.classList.contains('upsmon-type-' + t) ? '' : 'none';
	});
	document.getElementById('upsmon-shutdown').style.display = t === 'wake_only' ? 'none' : '';
}
upsmonType();
</script>
		<?php
	}
}

/* ----- Settings ----- */
if ($tab === 'settings') {
	$shown = $role === 'client' ? 'client' : 'server';
	echo '<form method="post" class="form-horizontal"><input type="hidden" name="tab" value="settings">' .
		'<input type="hidden" name="act" value="save_settings"><input type="hidden" name="shown_role" value="' . $shown . '">';

	upsmon_panel_open('Role');
	upsmon_row('This firewall is a', upsmon_select('role', array('server' => 'Server', 'client' => 'Client'), $role === '' ? 'server' : $role),
		'<b>Server</b>: reads NUT, shuts machines down in order and wakes them afterwards (the usual choice for pfSense). ' .
		'<b>Client</b>: only takes shutdown orders from another UPS Monitor server. Save after changing this to see its settings.');
	upsmon_panel_close();

	if ($shown === 'server') {
		$srv = $s['server'] ?? upsmon_default_server();
		$t = $srv['self_tests'] ?? array();
		upsmon_panel_open('NUT');
		upsmon_row('Host', upsmon_input('nut_host', $srv['nut']['host'] ?? '127.0.0.1'), '127.0.0.1 if the NUT package runs on this firewall.');
		upsmon_row('Port', upsmon_input('nut_port', $srv['nut']['port'] ?? 3493, 'number'));
		upsmon_row('Username', upsmon_input('nut_user', $srv['nut']['username'] ?? ''), 'Optional. Only needed for self-tests on most setups.');
		upsmon_row('Password', upsmon_secret('nut_pass', $srv['nut']['password'] ?? ''));
		upsmon_row('Check every', upsmon_input('poll', $srv['poll_seconds'] ?? 10, 'number', 'min="1"'), 'Seconds between UPS checks.');
		upsmon_panel_close();

		upsmon_panel_open('Server');
		upsmon_row('Port for clients', upsmon_input('listen_port', $srv['listen_port'] ?? 3494, 'number'), 'TCP port that paired PCs connect to.');
		upsmon_row('', upsmon_check('wake_after_restart', !empty($srv['wake_after_restart']), 'Wake machines after this monitor restarts (the firewall may have lost power too)'));
		upsmon_row('Discord webhook', upsmon_input('webhook', $srv['webhook_url'] ?? ''), 'Gets a message for every shutdown, wake and failure. Machines can override it.');
		upsmon_panel_close();

		upsmon_panel_open('UPS self-tests');
		upsmon_row('', upsmon_check('st_enabled', !empty($t['enabled']), 'Run battery self-tests on a schedule'));
		upsmon_row('Test type', upsmon_select('st_type', array('quick' => 'Quick', 'deep' => 'Deep'), $t['type'] ?? 'quick'));
		upsmon_row('Every', upsmon_input('st_months', $t['every_months'] ?? 3, 'number', 'min="1"'), 'Months between tests.');
		$last = array();
		foreach ($st['last_self_test'] ?? array() as $u => $when) {
			$last[] = upsmon_h($u) . ' ' . upsmon_ago($when);
		}
		upsmon_row('UPS names', upsmon_input('st_ups', implode(', ', $t['ups'] ?? array())),
			'Comma separated. Needs a NUT user (set under NUT above) that may run instant commands. With pfSense\'s NUT package, add one under ' .
			'<b>Services &gt; UPS &gt; Settings &gt; Advanced settings</b>, in "Additional configuration lines for upsd.users".' .
			($last ? ' Last tests: ' . implode(', ', $last) . '.' : ''));
		upsmon_row('Discord webhook', upsmon_input('st_webhook', $t['webhook_url'] ?? ''), 'Optional. Leave empty to use the one above.');
		upsmon_panel_close();
	} else {
		$c = $s['client'] ?? upsmon_default_client();
		upsmon_panel_open('Server connection');
		upsmon_row('Pairing code', upsmon_input('pairing_code', '', 'text', 'placeholder="upsmon://..."'),
			'Paste the code from the server\'s Machines tab. This fills in the fields below.');
		upsmon_row('Server address', upsmon_input('server_address', $c['server_address'] ?? ''));
		upsmon_row('Client name', upsmon_input('client_name', $c['name'] ?? ''));
		upsmon_row('Client key', upsmon_secret('client_key', $c['key'] ?? ''));
		upsmon_row('Server fingerprint', upsmon_input('server_fingerprint', $c['server_fingerprint'] ?? ''));
		upsmon_panel_close();
		upsmon_panel_open('Client');
		upsmon_row('Failsafe', upsmon_input('failsafe', $c['failsafe_seconds'] ?? 60, 'number', 'min="0"'),
			'If the server goes silent while the UPS is on battery, shut down after this many seconds. 0 turns it off.');
		upsmon_row('Discord webhook', upsmon_input('webhook', $c['webhook_url'] ?? ''));
		upsmon_panel_close();
	}
	echo '<button class="btn btn-primary">Save</button></form>';
}
?>
<script>
function upsmonCopy(i, btn) {
	var el = document.getElementById('upsmon-code-' + i);
	el.select();
	var done = function () { btn.textContent = 'Copied'; };
	if (navigator.clipboard) {
		navigator.clipboard.writeText(el.value).then(done, function () { document.execCommand('copy'); done(); });
	} else {
		document.execCommand('copy');
		done();
	}
}
</script>
<?php
include("foot.inc");
