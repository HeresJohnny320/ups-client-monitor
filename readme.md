# 🔌 UPS Monitor & Wake-on-LAN

A lightweight, automated tool to manage your power safety. It monitors your UPS (via NUT) and takes action based on battery levels—keeping your data safe during outages and bringing your network back to life when power returns.

---

## 🚀 Quick Start

1. **Run the app:** Double-click the executable or run it from your terminal:
   ```bash
   ./ups-monitor
   ```
2. **Initial Setup:** On the first run, the app will ask if you want to be a **Server** or a **Client**. It will then create a configuration file and exit.
3. **Configure:** Open the `config.json` file (the app will tell you exactly where it saved it) and add your UPS and Network details.
4. **Deploy:** Run the app again to start monitoring!

---

## 🛠 Which Mode Should I Use?

### 🏠 Client Mode ("Save My Data")
**Best for:** Workstations, Gaming PCs, or File Servers.
*   **What it does:** Watches a specific UPS. If the power goes out and the battery drops **to or below** your limit, it triggers a safe shutdown.
*   **Settings:** 
    * `client_ups_name`: The name of your UPS (e.g., "ups1").
    * `battery_percent`: The shutdown threshold (e.g., `25` means shut down at 25% or less).

### 🌐 Server Mode ("The Manager")
**Best for:** A Raspberry Pi or an "always-on" device.
*   **What it does:** Monitors your UPS units. When the power comes back on and the batteries have recharged **at or above** your limit, it sends a "Magic Packet" to wake up your other computers.
*   **Settings:** 
    * `wake_targets`: A list of computers you want to wake up, including their MAC addresses and the charge level required to wake them (e.g., `80` means wait for 80% charge before waking).

---

## ⚙️ Configuration Locations

The app automatically saves your settings in your system's standard config folder:
- **Windows:** `%AppData%\ups-monitor\config.json`
- **Linux:** `~/.config/ups-monitor/config.json`
- **macOS:** `~/Library/Application Support/ups-monitor/config.json`

---

## 💡 Important Requirements
*   **NUT Server:** You must have a Network UPS Tools (NUT) server running and accessible.
*   **Wake-on-LAN:** For Server mode to work, the target computers must have "Wake on Magic Packet" enabled in their BIOS and Network settings.
*   **Shutdown Permissions:** On Linux/macOS, the app may need administrative privileges (sudo) to trigger a system shutdown in Client mode.

---

**Would you like me to show you how to set this up to start automatically when your computer boots up?**
