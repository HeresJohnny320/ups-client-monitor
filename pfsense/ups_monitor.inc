<?php
/*
 * ups_monitor.inc
 *
 * Shared helpers for the UPS Monitor page and dashboard widget on pfSense.
 * Installed to /usr/local/pkg by "ups-monitor -install-pfsense".
 *
 * These pages never edit settings themselves. They talk to the running monitor
 * over its control socket, and the monitor validates, saves and applies every change,
 * exactly like the terminal UI does.
 */

define('UPSMON_SOCKET', '/usr/local/etc/ups-monitor/control.sock');
define('UPSMON_RC', '/usr/local/etc/rc.d/ups_monitor.sh');

$upsmon_types = array(
	'proxmox' => 'Proxmox VE',
	'truenas' => 'TrueNAS',
	'client' => 'Client (this app)',
	'wake_only' => 'Wake-on-LAN only',
);

/* Sends one request to the monitor and returns its decoded reply. */
function upsmon_call($req) {
	$sock = @stream_socket_client('unix://' . UPSMON_SOCKET, $errno, $errstr, 3);
	if (!$sock) {
		return array('ok' => false, 'down' => true, 'error' => "The UPS Monitor service isn't running.");
	}
	// Proxmox/TrueNAS login tests can take up to ~20 seconds.
	stream_set_timeout($sock, 45);
	fwrite($sock, json_encode($req, JSON_UNESCAPED_SLASHES) . "\n");
	$raw = stream_get_contents($sock);
	fclose($sock);
	$resp = json_decode($raw, true);
	if (!is_array($resp)) {
		return array('ok' => false, 'error' => 'No answer from the UPS Monitor service.');
	}
	if (empty($resp['ok']) && empty($resp['error'])) {
		$resp['error'] = 'The UPS Monitor service returned an error.';
	}
	return $resp;
}

function upsmon_h($v) {
	return htmlspecialchars((string)$v, ENT_QUOTES | ENT_SUBSTITUTE, 'UTF-8');
}

/* "3m ago" style text for a Go timestamp; Go's zero time means never. */
function upsmon_ago($ts) {
	$t = strtotime((string)$ts);
	if ($t === false || $t <= 0 || strpos((string)$ts, '0001-') === 0) {
		return 'never';
	}
	$d = max(0, time() - $t);
	if ($d < 60) {
		return $d . 's ago';
	}
	if ($d < 3600) {
		return floor($d / 60) . 'm ago';
	}
	if ($d < 86400) {
		return floor($d / 3600) . 'h ' . floor(($d % 3600) / 60) . 'm ago';
	}
	return floor($d / 86400) . 'd ago';
}

function upsmon_on_battery($status) {
	return strpos($status, 'OB') !== false || strpos($status, 'LB') !== false;
}

/* A client key in the same format the Go side makes: 32 random bytes, base64url. */
function upsmon_new_key() {
	return rtrim(strtr(base64_encode(random_bytes(32)), '+/', '-_'), '=');
}

/* Reads upsmon://host:port?id=..&key=..&fp=.. (line breaks from copying are ignored). */
function upsmon_parse_pairing($code) {
	$code = preg_replace('/\s+/', '', (string)$code);
	$u = parse_url($code);
	if (!is_array($u) || ($u['scheme'] ?? '') !== 'upsmon' || empty($u['host'])) {
		return null;
	}
	parse_str($u['query'] ?? '', $q);
	if (empty($q['id']) || empty($q['key']) || empty($q['fp'])) {
		return null;
	}
	$address = $u['host'] . (isset($u['port']) ? ':' . $u['port'] : '');
	return array('server_address' => $address, 'name' => $q['id'], 'key' => $q['key'], 'server_fingerprint' => $q['fp']);
}

function upsmon_charge_bar($u) {
	if (empty($u['charge_known'])) {
		return '<span class="text-muted">unknown</span>';
	}
	$c = (int)$u['charge'];
	$class = $c >= 50 ? 'progress-bar-success' : ($c >= 25 ? 'progress-bar-warning' : 'progress-bar-danger');
	return '<div class="progress" style="margin-bottom:0; min-width:120px">' .
		'<div class="progress-bar ' . $class . '" role="progressbar" style="width:' . $c . '%">' . $c . '%</div></div>';
}

function upsmon_power_label($status) {
	if (strpos($status, 'LB') !== false) {
		return '<span class="label label-danger">LOW BATTERY</span>';
	}
	if (strpos($status, 'OB') !== false) {
		return '<span class="label label-warning">On battery</span>';
	}
	if ($status === '') {
		return '<span class="label label-default">unknown</span>';
	}
	return '<span class="label label-success">On mains</span>';
}

/* The UPS table, shared by the page and the widget. */
function upsmon_ups_table($st) {
	$ups = $st['ups'] ?? array();
	if (empty($ups)) {
		return '<p class="text-muted">No UPS seen yet.</p>';
	}
	usort($ups, function ($a, $b) { return strcmp($a['name'], $b['name']); });
	$out = '<table class="table table-striped table-condensed"><thead><tr><th>UPS</th><th>Power</th><th>Battery</th><th>NUT status</th></tr></thead><tbody>';
	foreach ($ups as $u) {
		$out .= '<tr><td>' . upsmon_h($u['name']) . '</td><td>' . upsmon_power_label($u['status']) .
			'</td><td>' . upsmon_charge_bar($u) . '</td><td><code>' . upsmon_h($u['status']) . '</code></td></tr>';
	}
	return $out . '</tbody></table>';
}

/* The machine table: shutdown and wake state for each machine. */
function upsmon_machine_table($st, $compact = false) {
	global $upsmon_types;
	$machines = $st['machines'] ?? array();
	if (empty($machines)) {
		return '<p class="text-muted">No machines yet. Add them on the Machines tab.</p>';
	}
	$out = '<table class="table table-striped table-condensed"><thead><tr><th>Machine</th>';
	if (!$compact) {
		$out .= '<th>Type</th><th>UPS</th>';
	}
	$out .= '<th>Shutdown</th><th>Wake</th></tr></thead><tbody>';
	foreach ($machines as $m) {
		$off = ($m['shutdown_state'] ?? '') === '' ? '<span class="text-muted">—</span>' :
			'≤ ' . (int)$m['shutdown_at'] . '%: ' . upsmon_h($m['shutdown_state']);
		$on = ($m['wake_state'] ?? '') === '' ? '<span class="text-muted">—</span>' :
			'≥ ' . (int)$m['wake_at'] . '%: ' . upsmon_h($m['wake_state']);
		$out .= '<tr><td>' . upsmon_h($m['name']) . '</td>';
		if (!$compact) {
			$out .= '<td>' . upsmon_h($upsmon_types[$m['type']] ?? $m['type']) . '</td><td>' . upsmon_h($m['ups']) . '</td>';
		}
		$out .= '<td>' . $off . '</td><td>' . $on . '</td></tr>';
	}
	return $out . '</tbody></table>';
}

function upsmon_connection_line($st) {
	if (($st['role'] ?? '') === '') {
		return '<span class="text-warning">Not set up yet. Open the Settings tab to get started.</span>';
	}
	$what = ($st['source'] ?? '') === 'server' ? 'Server' : 'NUT';
	$state = !empty($st['nut_connected']) ? '<span class="text-success">connected</span>' :
		'<span class="text-danger">not connected</span>' . (($st['nut_error'] ?? '') !== '' ? ' <span class="text-muted">(' . upsmon_h($st['nut_error']) . ')</span>' : '');
	$line = $what . ' <code>' . upsmon_h($st['nut_host']) . '</code>: ' . $state;
	if ($what === 'NUT') {
		$line .= ' · last check ' . upsmon_ago($st['last_poll']);
	}
	return $line;
}
