# Running meshbridge on Linux

This runs a long-lived bridge as a systemd service on any Linux machine on the same network as a WiFi (or Ethernet) MeshCore companion node. The machine can be x86-64, arm64 or a Raspberry Pi. USB nodes need serial support, which isn't built yet.

## Quick install

```sh
curl -fsSL https://raw.githubusercontent.com/sierragridteam/sierra-meshrelay/main/scripts/install.sh | sh
```

This does steps 1–3 below in one go:
- downloads the latest release for this machine's CPU and checks its SHA-256 against the release's `SHA256SUMS`,
- installs the binary to `/usr/local/bin`,
- pairs with SIERRA (asking for your passphrase, or showing a pairing code),
- installs and starts the service.

It runs as your user and uses `sudo` only for the steps that need root, so read it first if you like ([scripts/install.sh](../scripts/install.sh)). Running it again upgrades the bridge. An already-paired bridge isn't paired again.

Options go after `sh -s --`:

```sh
curl -fsSL …/install.sh | sh -s -- --passphrase "maple river quartz otter"   # pre-approved; no prompts
curl -fsSL …/install.sh | sh -s -- --host 192.168.1.50                        # node on another subnet
curl -fsSL …/install.sh | sh -s -- --version v0.4.0                           # a specific release
curl -fsSL …/install.sh | sh -s -- --no-service                               # pair, but don't install the service
```

With `--passphrase`, nobody needs to be at the keyboard, so it suits Pi images and installing a batch of machines with a multi-use invite. Without a terminal and without a passphrase, it prints a pairing code and waits for approval.

To do it step by step instead:

## 1. Download

```sh
curl -LO https://github.com/sierragridteam/sierra-meshrelay/releases/latest/download/meshbridge_linux_amd64.tar.gz
tar xzf meshbridge_linux_amd64.tar.gz
./meshbridge version
```

Use `meshbridge_linux_arm64` for a 64-bit Pi, or `armv7`/`armv6` for 32-bit Raspberry Pi OS. Checksums are in `SHA256SUMS` on the same release.

## 2. Pair with SIERRA

```sh
sudo ./meshbridge setup
```

Setup scans this machine's network for a node on port 5000 and runs its identity check. If it finds more than one node, it asks which is yours. To point it at an address yourself, use `-host 192.168.1.50`.

It then asks for a passphrase from your SIERRA contact. If you have one, type it in any case, with spaces or hyphens. Otherwise press Enter to get a pairing code, and text that code to your SIERRA contact.

Once approved, setup writes `/etc/meshbridge/meshbridge.toml` (mode 0600, root only). That file holds the bridge's token and the node's pubkey.

## 3. Install the service

```sh
sudo ./meshbridge install
```

This copies the binary to `/usr/local/bin/meshbridge` and writes `/etc/systemd/system/meshbridge.service`, then enables and starts the service.

The service starts at boot after the network is up, and restarts 5 s after any exit. It runs as root, but sandboxed: everything is read-only except `/etc/meshbridge`, home directories aren't visible to it, and it can't gain privileges.

## 4. Check on it

```sh
sudo meshbridge status            # green / amber / red, node, last traffic, pause state
journalctl -u meshbridge -f       # logs (never frame payloads)
systemctl status meshbridge
```

`status` needs `sudo`, because it reads the control secret in `/etc/meshbridge/control.token`.

After restarting the node, `sudo meshbridge reconnect` looks for it now instead of at the bridge's next retry. While the node is unavailable, `status` says since when, why, and where it was last connected.

The bridge sends its recent log lines (never message content, and with secrets redacted) to the relay, where SIERRA admins can read them to help if something goes wrong. To turn that off, set `send_logs = false` under `[host]` in the config and restart the service.

## Pausing

```sh
sudo meshbridge pause 1h          # or: tomorrow (08:00), until-resumed
sudo meshbridge resume
```

Pausing frees the node's single connection, so you can use your own MeshCore app. SIERRA clients are told the gateway is paused until the time you gave. A pause with `until-resumed` survives restarts and reboots; a timed pause doesn't.

## Sharing a repeater

```sh
sudo meshbridge repeater add       # lists the repeaters your node has heard, by name; type a number, or part of a name to narrow the list
sudo meshbridge repeater add ridge # only the ones whose name contains "ridge"
sudo meshbridge repeater list
sudo meshbridge repeater remove "Ridge RPT"
```

SIERRA then logs in to the repeater over the mesh to read its status (battery, airtime, packet counts) for the Grid. Any SIERRA gateway in range may be used, not only yours. Give the guest password if the repeater has one: a guest can only read status. The relay checks the password by logging in before it stores it (encrypted). Removing a repeater deletes its password from SIERRA.

## When the node's address changes

The node gets its address from your router by DHCP. If that address changes, the bridge tries, in order:

1. the last address that worked (saved in `/etc/meshbridge/state.json`),
2. the configured address,
3. a scan of the local network for the node with the pubkey in its config.

If an address answers with a different node (two nodes on one network can swap addresses), the bridge forgets it and scans straight away. Otherwise it scans at most once a minute. If the node is on a different subnet from the bridge, set `node.tcp_host` in the config, or reserve the node's address on your router.

## Changing the node

To use a different node (a replacement, or the same one reflashed with a new identity), put it on this network and run:

```sh
sudo meshbridge change-node            # finds the nodes on this network and asks which one
sudo meshbridge change-node -host 192.168.1.60   # or name its address
```

It shows the switch and asks you to confirm, then tells SIERRA, saves the new node in the config and restarts the service. The gateway keeps its name, token and shared repeaters. SIERRA records the change in the gateway's logs. The new node hasn't heard your repeaters yet, so `repeater add` lists them once it hears their adverts.

## Upgrading

Bridges update themselves (v0.7.0 and later): when SIERRA rolls out a release, the bridge downloads it, checks its signature against the release key built into meshbridge, waits until nobody is using the node, and restarts into it. The service installs it before starting (`ExecStartPre … apply-update`). The journal shows each step. To turn it off, set `auto_update = false` under `[host]` in the config.

To upgrade by hand, run the installer again, or download the new release and run `sudo ./meshbridge install`. That replaces the binary and the unit, and restarts the service. The config, token and state stay as they are. A bridge installed before v0.7.0 needs this once to get the self-updating unit.

## Removing

```sh
sudo meshbridge uninstall         # stops and removes the service; keeps /etc/meshbridge
sudo rm -r /etc/meshbridge        # only if this machine is leaving SIERRA (ask your contact to remove the gateway)
```

## Troubleshooting

| You see | Do |
|---|---|
| `status` is red, "Can't reach SIERRA" | Check the machine's internet. The bridge keeps retrying with backoff, up to 1 min apart. |
| Red, "SIERRA didn't accept this bridge's credentials" | The token was rotated or revoked. Ask your SIERRA contact, then `sudo meshbridge setup -repair`. The bridge waits 15 min between attempts, so it doesn't trip the relay's lockout. |
| Red, "This isn't the node this bridge is set up for" | A different node answered. If you replaced the node, run `sudo meshbridge change-node`. Otherwise check the right node is on and on this network. |
| Amber, "your node isn't connected" | The node is off, out of WiFi range, or another app is connected to it (a node allows one connection). The logs show what the bridge tried. |
| `install`: "systemd (systemctl) not found" | Run `meshbridge run -config /etc/meshbridge/meshbridge.toml` under your own supervisor instead. |
