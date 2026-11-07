# Running meshbridge on Windows

This runs a long-lived bridge as a Windows service on any Windows 10 or 11 PC (x64 or ARM) on the same network as a WiFi (or Ethernet) MeshCore companion node. USB nodes need serial support, which isn't built yet. WSL isn't needed or used.

## Quick install

From an elevated PowerShell (right-click PowerShell, **Run as administrator**):

```powershell
irm https://raw.githubusercontent.com/sierragridteam/sierra-meshrelay/main/scripts/install.ps1 | iex
```

This does steps 1–3 below in one go:
- downloads the latest release for this PC's CPU and checks its SHA-256 against the release's `SHA256SUMS`,
- installs `meshbridge.exe` to `C:\Program Files\SIERRA\meshbridge` and adds that folder to the system `PATH`,
- pairs with SIERRA (asking for your passphrase, or showing a pairing code),
- installs and starts the service.

Read the script first if you like ([scripts/install.ps1](../scripts/install.ps1)). Running it again upgrades the bridge: it stops the service, replaces the binary and starts it again. An already-paired bridge isn't paired again.

To pass options, run the script as a script block:

```powershell
$mb = [scriptblock]::Create((irm https://raw.githubusercontent.com/sierragridteam/sierra-meshrelay/main/scripts/install.ps1))
& $mb -Passphrase "maple river quartz otter"   # pre-approved; no prompts
& $mb -NodeHost 192.168.1.50                   # node on another subnet
& $mb -Version v0.6.0                          # a specific release
& $mb -NoService                               # pair, but don't install the service
```

### Over SSH

Windows' OpenSSH server gives an administrator an elevated session, so the install works over SSH with no UAC prompt. Its default shell is `cmd`, so start PowerShell for it:

```bat
powershell -NoProfile -ExecutionPolicy Bypass -c "irm https://raw.githubusercontent.com/sierragridteam/sierra-meshrelay/main/scripts/install.ps1 | iex"
```

Connect with a terminal (`ssh` does this by default) so setup can ask for the passphrase, or pass `-Passphrase` and nobody needs to answer anything.

## 1. Download

Download `meshbridge_windows_amd64.zip` (or `meshbridge_windows_arm64.zip` on an ARM PC) from the [latest release](https://github.com/sierragridteam/sierra-meshrelay/releases/latest) and extract it. Checksums are in `SHA256SUMS` on the same release.

## 2. Pair with SIERRA

In an elevated PowerShell, in the extracted folder:

```powershell
.\meshbridge.exe setup
```

Setup scans this PC's network for a node on port 5000 and runs its identity check. If it finds more than one node, it asks which is yours. To point it at an address yourself, use `-host 192.168.1.50`.

It then asks for a passphrase from your SIERRA contact, or press Enter to get a pairing code to text them.

Once approved, setup writes `C:\ProgramData\SIERRA\meshbridge\meshbridge.toml`. That file holds the bridge's token, so setup first restricts the folder to SYSTEM and Administrators (ProgramData is otherwise readable by every user on the PC).

## 3. Install the service

```powershell
.\meshbridge.exe install
```

This copies the binary to `C:\Program Files\SIERRA\meshbridge\meshbridge.exe`, creates the `meshbridge` service ("SIERRA mesh bridge") and starts it.

- It starts automatically at boot (delayed start), with nobody logged in.
- It restarts after a crash or an error exit: 5 s, then 15 s, then every 60 s, with the count reset after a day.
- It runs as its own virtual account, `NT SERVICE\meshbridge`, not as SYSTEM. That account can change only the config folder, which `install` grants it.
- While it runs, it asks Windows not to sleep when idle (`host.keep_awake`, on by default). It doesn't keep the screen on, and choosing Sleep still sleeps.

## 4. Check on it

```powershell
meshbridge status                                                     # green / amber / red, node, last traffic, pause state
Get-Content -Wait C:\ProgramData\SIERRA\meshbridge\meshbridge.log     # logs (never frame payloads)
Get-Service meshbridge
powercfg /requests                                                    # lists the bridge's keep-awake request
```

`status` needs an elevated PowerShell, because it reads the control secret in the config folder. The log is capped at 5 MB, with the previous one kept as `meshbridge.log.1`.

The bridge sends its recent log lines (never message content, and with secrets redacted) to the relay, where SIERRA admins can read them to help if something goes wrong. To turn that off, set `send_logs = false` under `[host]` in the config and restart the service.

## Pausing

```powershell
meshbridge pause 1h          # or: tomorrow (08:00), until-resumed
meshbridge resume
```

Pausing frees the node's single connection, so you can use your own MeshCore app. See [linux.md](linux.md#pausing) for how pauses behave.

## Sharing a repeater

```powershell
meshbridge repeater add       # lists the repeaters your node has heard, by name; type a number, or part of a name to narrow the list
meshbridge repeater add ridge # only the ones whose name contains "ridge"
meshbridge repeater list
meshbridge repeater remove "Ridge RPT"
```

SIERRA then logs in to the repeater over the mesh to read its status (battery, airtime, packet counts) for the Grid. Any SIERRA gateway in range may be used, not only yours. Give the guest password if the repeater has one: a guest can only read status. The relay checks the password by logging in before it stores it (encrypted). Removing a repeater deletes its password from SIERRA.

## Upgrading

Bridges update themselves (v0.7.0 and later): when SIERRA rolls out a release, the service downloads it, checks its signature against the release key built into meshbridge, and once nobody is using the node marks it ready. A scheduled task, `SIERRA\meshbridge update`, runs every 5 minutes as SYSTEM, and installs a ready update: it stops the service, swaps the binary and starts it again. The log shows each step. To turn it off, set `auto_update = false` under `[host]` in the config.

To upgrade by hand, run the installer again. Or download the new release and run `.\meshbridge.exe install` from it, which stops the service, replaces the binary and starts it.

## Removing

```powershell
meshbridge uninstall                                     # stops and removes the service; keeps the config
Remove-Item -Recurse C:\ProgramData\SIERRA\meshbridge    # only if this PC is leaving SIERRA (ask your contact to remove the gateway)
Remove-Item -Recurse "C:\Program Files\SIERRA\meshbridge"
```

## Troubleshooting

See [linux.md](linux.md#troubleshooting) for what each status means. On Windows also:

| You see | Do |
|---|---|
| "Run this from an elevated PowerShell" | Right-click PowerShell, Run as administrator. Over SSH, log in as an administrator. |
| `install`: "the service started but stopped again" | Read `meshbridge.log` in the config folder; the last line says why. |
| SmartScreen or Defender blocks `meshbridge.exe` | The binary isn't code-signed yet. The installer checks its SHA-256 against the release. |
| The bridge goes offline overnight | The PC is sleeping anyway (a laptop on battery, or a power plan that ignores requests). Check `powercfg /requests`, or set the plan's sleep to Never. |
