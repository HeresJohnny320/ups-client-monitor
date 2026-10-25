#!/bin/sh
#
# Installs or updates UPS Monitor on pfSense. Run it as root (SSH, or Diagnostics > Command Prompt):
#
#   fetch -o - https://raw.githubusercontent.com/HeresJohnny320/ups-client-monitor/main/pfsense/install.sh | sh
#
# Running it again updates to the newest release and keeps your settings.
#
# This hands over to the main installer, which works on pfSense too: it downloads the latest
# release, checks it against the release's checksums, and "ups-monitor -install" then sets
# up the web page, widget and service. (Older instructions add "-s alpha"; that's ignored.)

set -e
fetch -q -o - "https://raw.githubusercontent.com/HeresJohnny320/ups-client-monitor/main/install.sh" | sh
