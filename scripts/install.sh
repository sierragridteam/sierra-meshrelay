#!/bin/sh
# Installs meshbridge on Linux: downloads the release for this machine,
# verifies its checksum, pairs it with SIERRA and installs the systemd
# service. Run it again to upgrade (an already-paired bridge skips pairing).
#
#   curl -fsSL https://raw.githubusercontent.com/sierragridteam/sierra-meshrelay/main/scripts/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --passphrase "maple river quartz otter"
#
# Options:
#   --passphrase WORDS   pair with the passphrase from your SIERRA contact
#                        (otherwise you're asked, or shown a pairing code)
#   --host ADDR          your node's address, if setup can't find it on the LAN
#   --port N             your node's TCP port (default 5000)
#   --version vX.Y.Z     install that release instead of the latest
#   --no-service         install and pair, but don't install the systemd service
#
# Advanced (testing, private relays):
#   --relay URL  --config PATH  --bin-dir DIR  --base-url URL
#
# Everything is inside main(), called on the last line, so a download cut
# short runs nothing.

set -eu

main() {
	base_url="https://github.com/sierragridteam/sierra-meshrelay/releases"
	version=latest
	bin_dir=/usr/local/bin
	config=/etc/meshbridge/meshbridge.toml
	service=yes
	setup_args=""

	while [ $# -gt 0 ]; do
		case "$1" in
		--passphrase) need_arg "$@"; setup_args="$setup_args --passphrase $(quote "$2")"; shift 2 ;;
		--host) need_arg "$@"; setup_args="$setup_args -host $(quote "$2")"; shift 2 ;;
		--port) need_arg "$@"; setup_args="$setup_args -port $(quote "$2")"; shift 2 ;;
		--relay) need_arg "$@"; setup_args="$setup_args -relay $(quote "$2")"; shift 2 ;;
		--version) need_arg "$@"; version=$2; shift 2 ;;
		--config) need_arg "$@"; config=$2; shift 2 ;;
		--bin-dir) need_arg "$@"; bin_dir=$2; shift 2 ;;
		--base-url) need_arg "$@"; base_url=$2; shift 2 ;;
		--no-service) service=no; shift ;;
		-h | --help) sed -n '2,20p' "$0" 2>/dev/null || echo "see https://github.com/sierragridteam/sierra-meshrelay/blob/main/docs/linux.md"; exit 0 ;;
		*) fail "unknown option: $1" ;;
		esac
	done

	[ "$(uname -s)" = Linux ] || fail "this installer is for Linux. See https://github.com/sierragridteam/sierra-meshrelay for other systems."
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	armv7l | armv7*) arch=armv7 ;;
	armv6l | armv6*) arch=armv6 ;;
	*) fail "unsupported CPU: $(uname -m)" ;;
	esac
	asset="meshbridge_linux_${arch}.tar.gz"
	if [ "$version" = latest ]; then
		url="$base_url/latest/download"
	else
		url="$base_url/download/$version"
	fi

	if [ "$(id -u)" -eq 0 ]; then
		sudo=""
	elif command -v sudo >/dev/null 2>&1; then
		sudo=sudo
	else
		fail "run this as root, or install sudo"
	fi

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	say "Downloading meshbridge ($version, linux/$arch)…"
	fetch "$url/$asset" "$tmp/$asset"
	fetch "$url/SHA256SUMS" "$tmp/SHA256SUMS"
	want=$(awk -v a="$asset" '$2 == a { print $1 }' "$tmp/SHA256SUMS")
	[ -n "$want" ] || fail "no checksum for $asset in SHA256SUMS"
	got=$(sha256 "$tmp/$asset")
	[ "$want" = "$got" ] || fail "checksum mismatch for $asset (got $got, want $want); not installing"
	tar -xzf "$tmp/$asset" -C "$tmp" ./meshbridge

	$sudo mkdir -p "$bin_dir"
	$sudo cp "$tmp/meshbridge" "$bin_dir/meshbridge.new"
	$sudo chmod 0755 "$bin_dir/meshbridge.new"
	$sudo mv "$bin_dir/meshbridge.new" "$bin_dir/meshbridge"
	mb="$bin_dir/meshbridge"
	say "Installed $("$mb" version | head -n 1) to $mb"

	# Releases since v0.4.1 accept -installer (setup then says the service is
	# installed next, rather than telling you to start the bridge by hand).
	if "$mb" setup -h 2>&1 | grep -q -- '-installer'; then
		setup_args="$setup_args -installer"
	fi

	if $sudo grep -qs '^token = "smb_' "$config"; then
		say "This bridge is already paired ($config); skipping setup."
	else
		say ""
		# With `curl | sh`, stdin is this script: give setup the terminal, so
		# it can ask for the passphrase or which node is yours. Without one,
		# setup still works: it shows a pairing code and waits for approval.
		if [ -r /dev/tty ] && (exec </dev/tty) 2>/dev/null; then
			eval "$sudo \"\$mb\" setup -config \"\$config\" $setup_args" </dev/tty
		else
			eval "$sudo \"\$mb\" setup -config \"\$config\" $setup_args" </dev/null
		fi
	fi

	if [ "$service" = yes ]; then
		$sudo "$mb" install -config "$config"
	else
		say ""
		say "Skipped the service. Run the bridge with: $sudo $mb run -config $config"
	fi
}

say() { printf '%s\n' "$*"; }
fail() {
	printf 'meshbridge installer: %s\n' "$*" >&2
	exit 1
}
need_arg() { [ $# -ge 2 ] || fail "$1 needs a value"; }

# quote wraps a value in single quotes for eval.
quote() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --proto '=https,http' -o "$2" "$1" || fail "download failed: $1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1" || fail "download failed: $1"
	else
		fail "needs curl or wget"
	fi
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{ print $1 }'
	else
		fail "needs sha256sum or shasum to verify the download"
	fi
}

main "$@"
