# UPS Monitor

I wanted one small box to keep an eye on my UPSes and take care of everything else when the power goes out: shut my servers down in a sensible order before the batteries run flat, and turn them back on once the power has been back for a while. This is that.

It reads your UPSes from a [NUT](https://networkupstools.org) server and can:

- shut down Proxmox VE and TrueNAS through their own APIs (nothing to install on them)
- shut down Windows, Linux and macOS machines that run this app in client mode
- wake everything back up with Wake-on-LAN after the outage, in the order you choose

Each machine gets its own battery level, so on a long outage the less important stuff goes down first and the important stuff stays up as long as possible.

## How it fits together

```
          NUT (upsd)
              │
              ▼
     ┌─────────────────┐ ──── shutdown order ────▶  client PCs (this app)
     │     server      │ ──── Proxmox / TrueNAS API ──▶  shut down
     │  (e.g. a Pi)    │ ──── Wake-on-LAN ──────────▶  everything, once power is back
     └─────────────────┘
```

You run it in **server** mode on something that stays on, like a Raspberry Pi plugged into the UPS. The server is the only thing that talks to NUT, and it makes all the decisions.

Anything else that should shut itself down cleanly runs it in **client** mode. A client only needs a pairing code from the server. It never listens on the network: it makes a single outgoing, encrypted connection to the server, and the only thing the server can tell it to do is shut down.

## Getting started

Grab a build for your platform from the [Releases](../../releases) page, or build it yourself (see the end of this page).

### Setting up the server

Run it in a terminal:

```bash
./ups-monitor
```

It'll offer to start the monitor in the background. Say yes, pick **Server**, then:

1. Under **General**, point it at your NUT server. The NUT username and password are optional; you only need them for battery self-tests.
2. Under **Machines**, add each thing it should look after. For each one, pick which UPS powers it, the battery % to shut it down at, and (optionally) its MAC address and the battery % to wake it back up at.
3. Check **Power Plan** to see it all laid out per UPS.

Every save takes effect straight away. When you quit with `q`, the monitor keeps running in the background; run `./ups-monitor` again whenever you want to check on it or change something.

### Setting up a client

On the server, add the PC under **Machines** with the type set to *Client (this app)*. When you save, it shows a pairing code that looks like `upsmon://192.168.1.5:3494?...`.

On the PC, run `./ups-monitor`, pick **Client**, go to **Server Connection** and paste the code. That's it. The client's Status screen should show it's connected, plus the battery level the server will shut it down at.

The code includes the client's secret key, so treat it like a password. To copy it out of the terminal, hold Shift while you select it with the mouse.

The server needs TCP port 3494 open so clients can reach it (you can change the port under General). Clients don't need any ports open.

### Running the server on pfSense

If your UPS is plugged into your pfSense box (or pfSense already runs the NUT package), it makes a good server, and you get a proper page in the pfSense web UI to run it from.

Log in to pfSense over SSH (or open **Diagnostics → Command Prompt**) and run:

```sh
fetch -o - https://raw.githubusercontent.com/HeresJohnny320/ups-client-monitor/main/pfsense/install.sh | sh
```

That downloads the right build for your box (amd64, or arm64 for Netgate ARM appliances), checks it against the release's checksums, and sets everything up:

- a page under **Services → UPS Monitor** with Status, Power Plan, Machines and Settings tabs
- a **UPS Monitor** dashboard widget (add it from the dashboard's **+** menu)
- an entry on **Status → Services**, so you can start, stop and restart it there, and it starts at boot

Then open **Services → UPS Monitor**, go to **Settings**, choose **Server** and point it at NUT. If the NUT package runs on the same box, the default of `127.0.0.1` is right. From there it works just like the terminal version: add your machines, check the Power Plan, and pair your PCs. Pairing codes have a **Copy** button.

A few pfSense specifics:

- To update, run the same command again. Your settings are kept.
- Settings live in `/usr/local/etc/ups-monitor`. pfSense's own config backups don't include that folder, so copy `settings.json` and the two `link-*.pem` files somewhere safe.
- The default LAN rule already lets PCs on your LAN reach port 3494. For PCs on other interfaces or VLANs, add a pass rule for TCP 3494 to "This Firewall". Don't open it on WAN.
- The terminal UI works on pfSense too: run `ups-monitor` over SSH.
- Battery self-tests need a NUT user that's allowed to run them. With pfSense's NUT package, go to **Services → UPS → Settings → Advanced settings** and add this to "Additional configuration lines for upsd.users" (pick your own password):

  ```
  [upsmonitor]
      password = pick-a-password
      instcmds = test.battery.start.quick
      instcmds = test.battery.start.deep
      instcmds = test.battery.start
  ```

  Then enter `upsmonitor` and that password as the NUT username and password under UPS Monitor's **Settings**.
- To remove it, run `ups-monitor -uninstall-pfsense`. Your settings folder is left in place.

## The power plan

Every machine is tied to one UPS and has a "shut down at" level. It can also have a MAC address and a "wake at" level. The **Power Plan** screen puts it all together per UPS, one row per machine:

```
⚡ ups1   on mains 100%
  Off order   Machine     Shuts down at       Wake order   Wakes at
  1st         dell r710   ≤ 90%               2nd          ≥ 60%
  2nd         truenas     ≤ 30%               1st          ≥ 40%
  —           proxmox2    off: left running   3rd          ≥ 80%
⚡ ups2   on mains 100%
  1st         gaming-pc   ≤ 40%               —            not woken (no MAC)
```

The higher a machine's shutdown %, the earlier it goes down; the lower its wake %, the earlier it comes back. Machines with the same % go together.

Each machine also has a **Shut down automatically** switch. Turn it off to leave that machine running during an outage, for example while you're working on it; it's still woken afterwards. On pfSense, the Machines list has a one-click **Turn shutdown off/on** button for this.

If NUT reports low battery (`LB`) before a machine's level is reached, it gets shut down anyway.

Machines are only woken after something happened to the power: either their UPS went on battery, or the server itself restarted (in case it lost power too; you can switch that off under General). So if you turn a PC off on purpose, it stays off. Once the UPS is back on mains and charged up to a machine's wake level, the server sends three wake packets, five minutes apart, in case the first one arrived while the machine was still shutting down.

A low wake level gets a machine back quickly. A high one waits until the battery could carry it through another outage.

### Making sure things actually come back on

Wake-on-LAN only works if the machine shut down cleanly and still has standby power, and it has to be enabled in the BIOS and on the network adapter. The wake packet goes out on every private network the server is on (your LAN and any VLANs), so leave the broadcast address empty unless you want to limit it to one network. If the UPS ran completely flat, the machine lost power entirely and won't see the packet. For that case, set the BIOS option "Restore on AC power loss" to "Power On". Setting both is the most reliable.

On Proxmox, set your VMs and containers to start at boot so they come back with the host.

## Proxmox and TrueNAS

The server shuts these down through their APIs. Each one gets a single shutdown request per outage; if the request fails, it's retried every minute. Use **Test Connection** on the machine's form to check your credentials. It only logs in and doesn't shut anything down.

Both systems ship with self-signed certificates, so leave "Verify TLS certificate" off unless you've installed a proper one.

### Proxmox VE

Create an API token that's only allowed to power nodes on and off:

1. **Datacenter → Permissions → Roles → Create.** Name it `UPSShutdown` and give it `Sys.PowerMgmt`.
2. **Datacenter → Permissions → API Tokens → Add.** User `root@pam`, token ID `ups`. Copy the secret now; Proxmox only shows it once.
3. **Datacenter → Permissions → Add → API Token Permission.** Path `/nodes/<your node>`, token `root@pam!ups`, role `UPSShutdown`.

In the machine's form, the API token is `root@pam!ups=<secret>` and the node name is the one shown in the Proxmox sidebar. The host is usually `https://<ip>:8006`.

Shutting down a node shuts its VMs and containers down gracefully, in their configured order. In a cluster, add each node as its own machine.

### TrueNAS

Go to **Settings (top-right menu) → API Keys → Add**, pick a user such as `truenas_admin`, and copy the key. Enter that user and the key in the machine's form.

The host has to start with `https://`. TrueNAS revokes any API key that's sent over plain HTTP, so the app refuses to try. It talks to the WebSocket API that TrueNAS 25.04 and later use (TrueNAS 26 dropped the old REST API).

## Client safety net

If a client loses contact with the server while its UPS is on battery (say the network switch wasn't on the UPS), it waits 60 seconds and then shuts itself down. You can change the delay under General on the client, or set it to 0 to turn this off.

## Trying it out safely

Set `UPS_MONITOR_DRY_RUN=1` and the app will log "would shut down now" instead of actually doing it:

```bash
UPS_MONITOR_DRY_RUN=1 ./ups-monitor              # Linux / macOS
$env:UPS_MONITOR_DRY_RUN=1; .\ups-monitor.exe    # Windows PowerShell
```

Then unplug the UPS for a bit and watch **Status & Logs**. This only covers the machine it runs on: Proxmox and TrueNAS will still really shut down, so test those with **Test Connection** instead.

## Settings and files

Everything is kept in your normal config folder:

- Linux: `~/.config/ups-monitor/`
- Windows: `%AppData%\ups-monitor\`
- macOS: `~/Library/Application Support/ups-monitor/`

In there you'll find:

| File | What it's for |
|---|---|
| `settings.json` | Your settings. Only your user can read it, since it holds passwords and API keys. |
| `state.json` | Things the app remembers for itself, like when each UPS was last self-tested. |
| `activity.log` | A record of every outage, shutdown, wake and self-test. |
| `link-cert.pem`, `link-key.pem` | The server's identity, which clients check before trusting it. Back these up; if you lose them, every client has to be paired again. |
| `control.sock` | How the UI talks to the running monitor. |

You don't have to touch `settings.json`, but it's plain JSON and the monitor picks up changes while it runs. If an edit doesn't make sense, it's logged and ignored and the old settings stay in place. Here's what a server's file looks like:

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

A machine's `type` is `proxmox`, `truenas`, `client` or `wake_only` (woken after outages but never shut down). The server's `webhook_url` gets a Discord message for everything that happens; give a machine its own `webhook_url` to send its messages somewhere else.

**Upgrading from the old version:** the old `config.json` isn't read anymore. It's left alone, and you'll need to set things up again in the UI. Until you do, the monitor just sits there and logs that it's waiting for setup.

## Running it at boot

The monitor notices when it's started without a terminal (by systemd, Task Scheduler and so on) and runs without the UI. You can also force that with `-daemon`.

### Linux (systemd)

Save this as `/etc/systemd/system/ups-monitor.service`, with `/path/to/` changed to wherever you put the binary:

```ini
[Unit]
Description=UPS Monitor
After=network.target

[Service]
ExecStart=/path/to/ups-monitor -daemon
Restart=always
User=root

[Install]
WantedBy=multi-user.target
```

Then:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now ups-monitor
```

Because the service runs as root, open the UI with `sudo /path/to/ups-monitor` so it finds the right settings. To stop the monitor, use `systemctl stop ups-monitor`; the "Stop Background Monitor" menu item won't stick, because systemd just starts it again.

### Windows

In an admin PowerShell:

```powershell
$action = New-ScheduledTaskAction -Execute "C:\path\to\ups-monitor.exe" -Argument "-daemon -config-dir C:\ProgramData\ups-monitor"
$trigger = New-ScheduledTaskTrigger -AtStartup
Register-ScheduledTask -Action $action -Trigger $trigger -TaskName "UPSMonitor" -User "SYSTEM" -RunLevel Highest
```

The task runs as SYSTEM, so both it and you need to use the same settings folder. To open the UI, run `C:\path\to\ups-monitor.exe -config-dir C:\ProgramData\ups-monitor` from an admin terminal.

### macOS (launchd)

Save this as `~/Library/LaunchAgents/com.upsmonitor.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.upsmonitor</string>
    <key>ProgramArguments</key>
    <array>
        <string>/path/to/ups-monitor</string>
        <string>-daemon</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
</dict>
</plist>
```

Then load it with `launchctl load ~/Library/LaunchAgents/com.upsmonitor.plist`.

## Command-line options

| Option | What it does |
|---|---|
| `-daemon` | Run the monitor without the UI. |
| `-config-dir <path>` | Use a different settings folder. |
| `-version` | Print the version and exit. |
| `-install-pfsense` | Install or update the pfSense page, widget and service (run as root on pfSense). |
| `-uninstall-pfsense` | Remove them again. Settings are kept. |

## Builds and releases

Every push to `main` gets tested on Linux, Windows and macOS and published as an alpha pre-release (`alpha-1`, `alpha-2`, …). Those haven't been tested on real hardware, so give them a dry run first. Tagged versions like `v1.0.0` are published as normal releases. `ups-monitor -version` tells you which one you're running.

There are builds for Linux (x86-64, plus arm64, armv7 and armv6 for Raspberry Pis), Windows (x64 and ARM), macOS (Intel and Apple Silicon) and FreeBSD (amd64 and arm64, which is what pfSense uses).

To build it yourself you need Go:

```bash
go build -o ups-monitor .
./build.sh          # or build every platform into ./build
```

## License

MIT. See [LICENSE](LICENSE).
