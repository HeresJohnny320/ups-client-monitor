<?php
/*
 * ups_monitor.widget.php
 *
 * Dashboard widget for UPS Monitor: battery level of each UPS and what each machine is doing.
 * Installed by "ups-monitor -install-pfsense". Add it from the dashboard's "+" (Available Widgets).
 */

require_once("guiconfig.inc");
require_once("/usr/local/pkg/ups_monitor.inc");
require_once("/usr/local/www/widgets/include/ups_monitor.inc");

function upsmon_widget_body() {
	$r = upsmon_call(array('cmd' => 'status'));
	if (empty($r['ok'])) {
		return '<!--upsmon--><div class="content"><p class="text-danger">' . upsmon_h($r['error']) .
			' <a href="ups_monitor.php">Open UPS Monitor</a></p></div>';
	}
	$st = $r['status'];
	return '<!--upsmon--><div class="content" style="padding-bottom:0"><p>' . upsmon_connection_line($st) . '</p></div>' .
		'<div class="table-responsive">' . upsmon_ups_table($st) . upsmon_machine_table($st, true) . '</div>';
}

if (isset($_GET['upsmon_refresh'])) {
	echo upsmon_widget_body();
	exit;
}
?>
<div id="upsmon-widget"><?=upsmon_widget_body()?></div>
<script>
(function () {
	var box = document.getElementById('upsmon-widget');
	setInterval(function () {
		fetch('/widgets/widgets/ups_monitor.widget.php?upsmon_refresh=1', {credentials: 'same-origin'})
			.then(function (r) { return r.ok ? r.text() : ''; })
			.then(function (html) {
				if (html.indexOf('<!--upsmon-->') === 0) { box.innerHTML = html; }
			});
	}, 10000);
})();
</script>
