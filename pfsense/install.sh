#!/bin/sh
#
# Installs or updates UPS Monitor on pfSense. Run it as root (SSH, or Diagnostics > Command Prompt):
#
#   fetch -o - https://raw.githubusercontent.com/HeresJohnny320/ups-client-monitor/main/pfsense/install.sh | sh
#
# That picks the newest release, or the newest alpha if there's no release with a pfSense
# build yet. To always take the newest alpha:
#
#   fetch -o - https://raw.githubusercontent.com/HeresJohnny320/ups-client-monitor/main/pfsense/install.sh | sh -s alpha
#
# Running it again updates to the newest version and keeps your settings.

set -e

REPO="HeresJohnny320/ups-client-monitor"
CHANNEL="${1:-release}"
PHP=/usr/local/bin/php

case "$(uname -m)" in
	amd64) ARCH=amd64 ;;
	arm64 | aarch64) ARCH=arm64 ;;
	*)
		echo "Sorry, $(uname -m) isn't supported. There are builds for amd64 and arm64."
		exit 1
		;;
esac
ASSET="ups-monitor-freebsd-$ARCH"

if [ "$(id -u)" != "0" ]; then
	echo "Please run this as root."
	exit 1
fi
if [ ! -x "$PHP" ] || [ ! -f /usr/local/www/guiconfig.inc ]; then
	echo "This doesn't look like pfSense."
	exit 1
fi

echo "Looking for the newest $CHANNEL build of $ASSET..."
# pfSense has no jq, but it always has PHP. Prints "<tag> <binary url> <checksums url>".
FOUND=$(fetch -q -o - "https://api.github.com/repos/$REPO/releases?per_page=50" | "$PHP" -r '
	$channel = $argv[1];
	$asset = $argv[2];
	$releases = json_decode(stream_get_contents(STDIN), true);
	if (!is_array($releases)) {
		exit(1);
	}
	// "release" prefers real releases and falls back to alphas; "alpha" takes the newest alpha.
	$passes = $channel === "alpha" ? array(true) : array(false, true);
	foreach ($passes as $allow_pre) {
		foreach ($releases as $r) {
			if (!empty($r["prerelease"]) && !$allow_pre) {
				continue;
			}
			if ($channel === "alpha" && empty($r["prerelease"])) {
				continue;
			}
			$bin = $sums = "";
			foreach ($r["assets"] as $a) {
				if ($a["name"] === $asset) {
					$bin = $a["browser_download_url"];
				}
				if ($a["name"] === "SHA256SUMS.txt") {
					$sums = $a["browser_download_url"];
				}
			}
			if ($bin !== "") {
				echo $r["tag_name"], " ", $bin, " ", $sums;
				exit(0);
			}
		}
	}
	exit(1);
' "$CHANNEL" "$ASSET") || true

if [ -z "$FOUND" ]; then
	echo "Couldn't find a build of $ASSET on GitHub. Check https://github.com/$REPO/releases"
	exit 1
fi
set -- $FOUND
TAG=$1
BIN_URL=$2
SUMS_URL=$3

TMP=$(mktemp -d /tmp/ups-monitor.XXXXXX)
trap 'rm -rf "$TMP"' EXIT

echo "Downloading $TAG..."
fetch -q -o "$TMP/$ASSET" "$BIN_URL"

if [ -n "$SUMS_URL" ]; then
	fetch -q -o "$TMP/SHA256SUMS.txt" "$SUMS_URL"
	WANT=$(awk -v f="$ASSET" '$2 == f { print $1 }' "$TMP/SHA256SUMS.txt")
	GOT=$(sha256 -q "$TMP/$ASSET")
	if [ -z "$WANT" ] || [ "$WANT" != "$GOT" ]; then
		echo "The download doesn't match its checksum. Not installing it."
		exit 1
	fi
	echo "Checksum OK."
fi

chmod 755 "$TMP/$ASSET"
if ! "$TMP/$ASSET" -h 2>&1 | grep -q install-pfsense; then
	echo "$TAG was built before pfSense support was added, so it can't be installed this way."
	echo "Try again once a newer release is out, or use: sh -s alpha"
	exit 1
fi
"$TMP/$ASSET" -install-pfsense
