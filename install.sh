#!/bin/sh
#
# Installs or updates UPS Monitor as a background service that starts at boot.
# Linux, macOS, FreeBSD, OpenBSD, NetBSD, DragonFly, illumos and Solaris. Run it as root:
#
#   curl -fsSL https://raw.githubusercontent.com/HeresJohnny320/ups-client-monitor/main/install.sh | sudo sh
#
# Running it again updates to the newest release and keeps your settings.
# (Windows: install.ps1. pfSense: this works too, or use pfsense/install.sh.)

set -e

REPO="HeresJohnny320/ups-client-monitor"
BASE="${UPS_MONITOR_DOWNLOAD_BASE:-https://github.com/$REPO/releases/latest/download}"

if [ "$(id -u)" != "0" ]; then
	echo "Please run this as root, for example:"
	echo "  curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | sudo sh"
	exit 1
fi

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
	linux | darwin | freebsd | openbsd | netbsd | dragonfly) ;;
	sunos)
		os=solaris
		if [ "$(uname -o 2>/dev/null)" = "illumos" ]; then os=illumos; fi
		;;
	*)
		echo "Sorry, $(uname -s) isn't supported. Windows users: see install.ps1."
		exit 1
		;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64 | amd64 | i86pc) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	i386 | i486 | i586 | i686) arch=386 ;;
	armv7*) arch=armv7 ;;
	armv6*) arch=armv6 ;;
	riscv64) arch=riscv64 ;;
	*)
		echo "Sorry, there's no build for $os on $arch."
		exit 1
		;;
esac
asset="ups-monitor-$os-$arch"

download() { # url file
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	elif command -v fetch >/dev/null 2>&1; then
		fetch -qo "$2" "$1"
	elif command -v ftp >/dev/null 2>&1; then
		ftp -Vo "$2" "$1" # OpenBSD's ftp also does https
	else
		echo "Need curl, wget or fetch to download."
		exit 1
	fi
}

sha256_of() { # file
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	elif command -v sha256 >/dev/null 2>&1; then
		sha256 -q "$1"
	elif command -v digest >/dev/null 2>&1; then
		digest -a sha256 "$1"
	else
		echo "Need sha256sum, shasum or sha256 to check the download." >&2
		exit 1
	fi
}

# Download next to where it's installed: some systems don't allow running programs from /tmp.
mkdir -p /usr/local/bin
TMP=$(mktemp -d /usr/local/bin/.ups-monitor-install.XXXXXX)
trap 'rm -rf "$TMP"' EXIT

echo "Downloading $asset..."
download "$BASE/$asset" "$TMP/ups-monitor"
download "$BASE/SHA256SUMS.txt" "$TMP/SHA256SUMS.txt"

want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$TMP/SHA256SUMS.txt")
got=$(sha256_of "$TMP/ups-monitor")
if [ -z "$want" ] || [ "$want" != "$got" ]; then
	echo "The download doesn't match its checksum. Not installing it."
	exit 1
fi
echo "Checksum OK."

chmod 755 "$TMP/ups-monitor"
"$TMP/ups-monitor" -install
