# 🔌 UPS Monitor & Wake-on-LAN

A lightweight, automated Go tool to manage power safety and network recovery. It monitors your UPS (via NUT) to trigger safe shutdowns during outages and sends Wake-on-LAN "Magic Packets" to restore your infrastructure when power returns.

---

## 🚀 Quick Start

1.  **Run the app:** Start the executable in your terminal:
    ```bash
    ./ups-monitor
    ```
2.  **Interactive Setup:** On the first run, follow the prompts to choose your mode (**Server** or **Client**). The app will generate a base config and exit.
3.  **Configure:** Open the `config.json` file in your system's config directory to fine-tune your settings.
4.  **Monitor:** Restart the app. It will now run the background monitor and open a command-line interface (CLI) for manual control.

---

## 🛠 Operation Modes

### 🌐 Server Mode ("The Manager")
**Best for:** Always-on devices like a Raspberry Pi.
*   **WOL Recovery:** Sends Magic Packets to "Wake Targets" once the UPS battery recharges to your specified limit.
*   **Auto Self-Tests:** Defaults to **Enabled**. Automatically triggers monthly UPS battery tests to ensure battery health.

### 🏠 Client Mode ("The Protector")
**Best for:** Workstations, Gaming PCs, or individual nodes.
*   **Safe Shutdown:** Monitors a specific UPS and triggers a system shutdown if the battery drops to your critical limit.
*   **Auto Self-Tests:** Defaults to **Disabled** to prevent unexpected power cycling on workstations.

---

## ⌨️ Command-Line Interface (CLI)

While the monitor runs in the background, you can type commands directly into the app:
*   `test`: Manually trigger a **quick** battery self-test.
*   `test-long`: Manually trigger a **deep** battery self-test.
*   `status`: Check the current connection and system status.
*   `exit`: Safely shut down the monitor and close the app.

---

## ⚙️ Configuration & Logs

The app stores everything in your system's standard config folder:
- **Windows:** `%AppData%\ups-monitor\`
- **Linux:** `~/.config/ups-monitor/`
- **macOS:** `~/Library/Application Support/ups-monitor/`

### Files:
*   `config.json`: Your settings, including NUT credentials, WOL targets, and test schedules.
*   `activity.log`: A persistent log of all power events, shutdowns, and self-test results.

---

## 💡 Requirements
*   **NUT Server:** A running [Network UPS Tools](https://networkupstools.org) server is required.
*   **Permissions:** On Linux/macOS, the app may need administrative privileges to execute the `shutdown` command.
*   **WOL Support:** Target machines must have **Wake-on-Magic-Packet** enabled in BIOS/UEFI and network adapter settings.
