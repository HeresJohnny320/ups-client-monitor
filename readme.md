# 🔌 UPS Monitor & Wake-on-LAN

A lightweight, automated Go tool to manage power safety and network recovery. It monitors your UPS (via NUT) to trigger safe shutdowns during outages and sends Wake-on-LAN "Magic Packets" to restore your infrastructure when power returns.

---

## 🚀 Quick Start

1.  **Run the app** in a terminal:
    ```bash
    ./ups-monitor
    ```
2.  **Start the monitor:** If none is running, it asks to start one in the background. Answer **Y**.
3.  **Set up in the TUI:** On first run, pick **Server** or **Client**, then fill in your NUT server and add your machines. Every **Save** is validated, written to `settings.json`, and **applied live**. No restart is needed.
4.  **Quit with `q`:** The monitor keeps running in the background. Run `./ups-monitor` again at any time to see live status or change settings. It finds the running monitor and attaches to it.

---

## 📥 Download

Prebuilt binaries for Linux (x86-64, Raspberry Pi arm64/armv7/armv6), Windows, macOS and FreeBSD are on the [Releases](../../releases) page.
*   **Alpha builds** (`alpha-N`, marked *Pre-release*) are published automatically on every push to `main`. They are tested in CI but not on real hardware. Try them with [dry run](#-testing-safely) first.
*   **Versioned releases** (`v1.0.0`, ...) are published when a version tag is pushed.

Run `ups-monitor -version` to see which build you have.

---

## 🛠 Operation Modes

```
                ┌──────────── NUT (upsd) ────────────┐
                │                                     │
                ▼  reads battery / status             │
        ┌──────────────┐  shutdown order   ┌──────────────────┐
        │    SERVER    │ ─────────────────▶│ CLIENT (Windows, │  one outbound TLS connection
        │ (e.g. a Pi)  │   (TLS, pinned)   │  Linux, macOS)   │  from the client; no open ports
        └──────────────┘                   └──────────────────┘
          │        │
          │        └── Proxmox / TrueNAS API ──▶ shutdown
          └── Wake-on-LAN magic packet ──▶ everything, once power is back
```

### 🌐 Server Mode ("The Manager")
**Best for:** Always-on devices like a Raspberry Pi. It is the only machine that talks to NUT.
*   **Decides for everyone:** Each client, Proxmox and TrueNAS server gets its own shutdown battery %, set on the server.
*   **Client shutdown:** Sends a shutdown order to linked clients. See [Linking Clients](#-linking-clients).
*   **Remote Shutdown (Proxmox / TrueNAS):** Shuts down Proxmox VE and TrueNAS through their APIs. Nothing is installed on them. See [Remote Shutdown](#-remote-shutdown-proxmox--truenas).
*   **Multiple UPSes:** Reads every UPS on your NUT server. Each machine is assigned to the UPS that powers it, so `ups2` can control your servers while `ups1` controls your PC.
*   **WOL Recovery:** After an outage, wakes machines in order as the UPS recharges. See [Power Plan](#-power-plan).
*   **Auto Self-Tests:** Optional scheduled UPS battery tests (off by default).

### 🏠 Client Mode ("The Protector")
**Best for:** Workstations, gaming PCs, Windows servers, anything that should shut down cleanly.
*   **Only needs the server:** Paste a pairing code from the server. The client doesn't need NUT details.
*   **Locked down:** The client never listens on the network. It makes one outbound, encrypted connection to the server, verifies it is *your* server, and the only command it accepts is **shutdown**. It cannot be told to run anything else, start anything, or change its settings remotely.
*   **Failsafe:** If the server goes silent while the UPS is on battery (for example, the network switch lost power), the client shuts itself down after a delay (60s by default, `0` turns it off).

---

## 🖥️ Terminal UI

Running `ups-monitor` in a terminal opens a menu-driven UI attached to the background monitor:

| Screen | What it does |
|---|---|
| **Status & Logs** | Live NUT (or server) connection, UPS charge/power state, each machine's state, and recent activity |
| **General** | Role (server/client). Server: NUT host/port/login, poll interval, client port, wake after restart, default Discord webhook. Client: failsafe and webhook |
| **Power Plan** *(server)* | Per UPS: the shutdown order on battery and the wake order when power returns. Flags machines that won't come back on |
| **Machines** *(server)* | Add, edit or delete machines (Proxmox, TrueNAS, client, Wake-on-LAN only). **Test Connection**, **Pairing Code**, **New Key**, **Wake Now** |
| **Server Connection** *(client)* | Paste the pairing code from the server |
| **UPS Self-Tests** *(server)* | Schedule, test type, which UPSes, webhook |
| **Run Actions** | Server: quick/deep self-test now, check Proxmox/TrueNAS logins, test webhooks. Client: test webhook |
| **Stop Background Monitor** | Stops monitoring until you start it again |

Keys: `↑/↓` move, `Enter`/`Tab` open a screen or next field, `Esc` back to the menu, `q` quit. The monitor keeps running after you quit, and the mouse works too.

Hand edits to `settings.json` are also picked up while the monitor runs. An invalid edit is logged and ignored, and the previous settings stay in effect.

### Options
*   `-daemon`: Run the monitor in the foreground with no UI (for services). This is automatic when there is no terminal, so existing service setups keep working.
*   `-config-dir <path>`: Use a different settings folder.
*   `-version`: Print the version.

---

## 🗺️ Power Plan

Every machine has a **UPS**, a **shut down at ≤ X%** (except wake-only machines), and optionally a **Wake-on-LAN MAC** with **wake at ≥ Y%**. All of it is on one form per machine. **Power Plan** shows the result per UPS:

```
⚡ ups2   on mains 100%
  On battery, as the charge drops:
    ≤  90%   shut down   truenas
    ≤  50%   shut down   pve1
  Power back, as the battery recharges:
    ≥  40%   wake        truenas
    ≥  80%   wake        pve1
⚡ ups1   on mains 100%
  On battery, as the charge drops:
    ≤  30%   shut down   desk
    ! desk has no Wake-on-LAN MAC, so it stays off after an outage
```

**How waking works:**
*   Machines are **only woken after a power event**: their UPS went on battery, or the server itself restarted (it may have lost power too; you can turn that off under General). A machine you turn off on purpose stays off.
*   Once that UPS is back on mains and recharged to the wake %, **3 packets are sent 5 minutes apart**, in case the first arrives while the machine is still shutting down. Then it stops until the next outage.
*   A low wake % brings a machine back quickly. A high one waits until the battery could ride out another outage.

**Best way to turn machines back on:**
*   **Wake-on-LAN** (built in) works when the machine shut down cleanly and still has standby power. Enable it in the BIOS/UEFI and the network adapter.
*   **BIOS "Restore on AC power loss" → Power On** covers the case where the UPS ran completely flat and the machine lost power entirely. Set **both** for reliable recovery.
*   **Proxmox:** set VMs/containers to *Start at boot* so they come up after the host wakes.

---

## 🔗 Linking Clients

1.  **On the server:** open **Machines** → **+ Add machine**, set **Type** to *Client (this app)*, and enter a name, the UPS it is powered by, and the battery % to shut it down at. Save. A **pairing code** appears (show it again later with **Pairing Code**):
    ```
    upsmon://192.168.1.5:3494?fp=…&id=desk&key=…
    ```
    It contains the server address, the client's secret key, and the server's certificate fingerprint. **Treat it like a password.** To copy it, select it with **Shift + mouse drag**. Line breaks picked up while copying are ignored.
2.  **On the client:** run `ups-monitor`, choose **Client**, open **Server Connection**, paste the code, and **Save & Connect**. The Status screen shows the server's UPS state and the shutdown % the server set.
3.  **Firewall:** allow inbound TCP **3494** on the server (configurable under General). Clients need no inbound ports.

**Security:**
*   The link is TLS 1.3. The client pins the server's certificate fingerprint, so a different machine pretending to be the server is refused.
*   Each client has its own random key. A wrong key is rejected and logged on the server.
*   **New Key** or **Delete** on the server disconnects that client immediately.
*   The server identity lives in `link-cert.pem` / `link-key.pem` in the server's settings folder. Keep these when moving or reinstalling the server, or every client must be paired again.

---

## 🧪 Testing Safely

Set `UPS_MONITOR_DRY_RUN=1` to log shutdowns instead of doing them. This works on clients. Proxmox/TrueNAS shutdowns are **not** affected, so use **Test Connection** for those.
```bash
UPS_MONITOR_DRY_RUN=1 ./ups-monitor        # Linux / macOS
$env:UPS_MONITOR_DRY_RUN=1; .\ups-monitor.exe   # Windows PowerShell
```
Then trigger a short outage (or pull the UPS plug) and watch **Status & Logs** on both machines.

---

## ⚙️ Settings & Logs

Everything lives in your system's standard config folder:
- **Windows:** `%AppData%\ups-monitor\`
- **Linux:** `~/.config/ups-monitor/`
- **macOS:** `~/Library/Application Support/ups-monitor/`

| File | What it is |
|---|---|
| `settings.json` | Your settings (owner-only, `0600`: it holds passwords and API tokens) |
| `state.json` | Data the monitor keeps for itself, such as last self-test dates |
| `activity.log` | Every power event, shutdown, wake and self-test result |
| `link-cert.pem` / `link-key.pem` *(server)* | The server's identity for paired clients. **Back these up** |
| `control.sock` | Private socket the UI uses to talk to the running monitor |

> **Upgrading from an older version:** the old `config.json` is **not read** (and is left untouched). Set things up again in the UI. The monitor logs a notice if it finds one.

The UI is the easy way to edit settings, but `settings.json` is meant to be readable. A server example:

```json
{
  "version": 2,
  "role": "server",
  "server": {
    "nut": { "host": "127.0.0.1", "port": 3493, "username": "monuser", "password": "secret" },
    "poll_seconds": 10,
    "listen_port": 3494,
    "wake_after_restart": true,
    "webhook_url": "https://discord.com/api/webhooks/…",
    "machines": [
      {
        "name": "truenas", "type": "truenas", "ups": "ups2", "shutdown_at_percent": 90,
        "truenas": { "host": "https://192.168.1.20", "username": "truenas_admin", "api_key": "1-AbC…", "verify_tls": false },
        "wake": { "mac": "aa:bb:cc:dd:ee:01", "at_percent": 40 }
      },
      {
        "name": "pve1", "type": "proxmox", "ups": "ups2", "shutdown_at_percent": 50,
        "proxmox": { "host": "https://192.168.1.10:8006", "node": "pve1", "token": "root@pam!ups=…", "verify_tls": false },
        "wake": { "mac": "aa:bb:cc:dd:ee:02", "at_percent": 80 }
      },
      { "name": "desk", "type": "client", "ups": "ups1", "shutdown_at_percent": 30, "client_key": "…",
        "wake": { "mac": "aa:bb:cc:dd:ee:03", "at_percent": 50 } },
      { "name": "printer", "type": "wake_only", "ups": "ups1", "wake": { "mac": "aa:bb:cc:dd:ee:04", "at_percent": 20 } }
    ],
    "self_tests": { "enabled": true, "type": "quick", "every_months": 3, "ups": ["ups1", "ups2"] }
  }
}
```

*   **Machine types:** `proxmox`, `truenas`, `client` (a PC running this app in client mode) and `wake_only` (never shut down, only woken).
*   **`webhook_url`:** The server's webhook gets every event. A machine's own `webhook_url` overrides it for that machine.
*   **NUT login:** Optional. Most setups only need it for self-tests, which require a NUT user allowed to run instant commands.

---

## 🔌 Proxmox & TrueNAS

The server shuts these down through their own API, so nothing is installed on them. Each gets one shutdown per outage. A failed request is retried every minute, and only the first failure is posted to Discord. A NUT `LB` (low battery) flag also triggers the shutdown. `verify_tls: false` accepts the self-signed certificates both systems ship with. Use **Test Connection** to check a token without shutting anything down.

### Proxmox VE API token
1. **Datacenter → Permissions → Roles → Create**: name `UPSShutdown`, privileges `Sys.PowerMgmt`.
2. **Datacenter → Permissions → API Tokens → Add**: user `root@pam`, Token ID `ups`. Copy the secret; it is shown only once.
3. **Datacenter → Permissions → Add → API Token Permission**: path `/nodes/<node>`, token `root@pam!ups`, role `UPSShutdown`.
4. Enter `root@pam!ups=<secret>` as the **API token** and the node name shown in the sidebar as **Node name**.

Shutting down a node stops its VMs/containers gracefully, following their shutdown order. For a cluster, add one machine per node.

### TrueNAS API key
1. Go to **Settings (top-right user menu) → API Keys → Add**, pick the user (e.g. `truenas_admin`), and copy the key.
2. Enter that user as **Username** and the key as **API key**.

This uses the JSON-RPC WebSocket API (`wss://<host>/api/current`, TrueNAS 25.04+), which is required on TrueNAS 26 because the REST API was removed. The host **must** be `https://`, because TrueNAS revokes API keys sent over plain HTTP.

---

## 💡 Requirements
*   **NUT Server:** A running [Network UPS Tools](https://networkupstools.org) server is required, and only the **server** connects to it.
*   **Network:** Clients must reach the server on TCP 3494.
*   **Permissions:** On Linux/macOS, the app may need administrative privileges to execute the `shutdown` command.
*   **WOL Support:** Target machines must have **Wake-on-Magic-Packet** enabled in BIOS/UEFI and network adapter settings.


## 🛠️ Run as a Background Service

To ensure the monitor starts automatically when your system boots, follow the steps for your operating system:

### 🐧 Linux (systemd)
1. **Create the service file:**
   ```bash
   sudo nano /etc/systemd/system/ups-monitor.service
   ```
2. **Paste this content** (update `/path/to/` to your actual folder):
   ```ini
   [Unit]
   Description=UPS Monitor & WOL Service
   After=network.target

   [Service]
   Type=simple
   ExecStart=/path/to/ups-monitor -daemon
   WorkingDirectory=/path/to/
   Restart=always
   User=root

   [Install]
   WantedBy=multi-user.target
   ```
3. **Enable and Start:**
   ```bash
   sudo systemctl daemon-reload
   sudo systemctl enable --now ups-monitor
   ```
4. **Change settings:** Run `sudo /path/to/ups-monitor`. The service runs as root, so open the UI as root to attach to it and use its `/root/.config/ups-monitor` settings. Don't use **Stop Background Monitor** here, because `Restart=always` brings it back. Use `systemctl stop` instead.

### 🪟 Windows (PowerShell)
Run this in **PowerShell (Admin)** to create a background task that starts at boot:
```powershell
$action = New-ScheduledTaskAction -Execute "C:\path\to\ups-monitor.exe" -Argument "-daemon -config-dir C:\ProgramData\ups-monitor" -WorkingDirectory "C:\path\to\"
$trigger = New-ScheduledTaskTrigger -AtStartup
Register-ScheduledTask -Action $action -Trigger $trigger -TaskName "UPSMonitor" -User "SYSTEM" -RunLevel Highest
```
To change settings, open an **Admin** terminal and run `C:\path\to\ups-monitor.exe -config-dir C:\ProgramData\ups-monitor`. The shared folder lets your admin terminal reach the monitor, which runs as SYSTEM.

### 🍎 macOS (launchd)
1. **Create the config file:**
   ```bash
   nano ~/Library/LaunchAgents/com.upsmonitor.plist
   ```
2. **Paste this content** (update `/path/to/` to your actual folder):
   ```xml
   <?xml version="1.0" encoding="UTF-8"?>
   <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://apple.com">
   <plist version="1.0">
   <dict>
       <key>Label</key>
       <string>com.upsmonitor</string>
       <key>ProgramArguments</key>
       <array>
           <string>/path/to/ups-monitor</string>
       </array>
       <key>RunAtLoad</key>
       <true/>
       <key>KeepAlive</key>
       <true/>
   </dict>
   </plist>
   ```
3. **Load it:**
   ```bash
   launchctl load ~/Library/LaunchAgents/com.upsmonitor.plist
   ```