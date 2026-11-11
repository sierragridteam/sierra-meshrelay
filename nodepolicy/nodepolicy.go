// Package nodepolicy decides which companion frames a remote session may
// exchange with a gateway's node.
//
// MeshCore companion firmware trusts whoever holds the companion link as the
// node's owner, sitting next to it. The relay breaks that assumption: a lease
// holder is somewhere else. So some commands are refused for every remote
// session, admins and the node's owner included:
//
//   - CMD_EXPORT_PRIVATE_KEY (23): returns the node's 64-byte private key.
//     Upstream MeshCore and wadamesh build with ENABLE_PRIVATE_KEY_EXPORT=1.
//   - CMD_IMPORT_PRIVATE_KEY (24): replaces the node's identity.
//   - CMD_FACTORY_RESET (51): wipes the node.
//   - CMD_RUN_CLI_COMMAND (66) with `get wifi.pwd`: returns the WiFi password
//     a WiFi/TCP node stores, which is usually the host's home network.
//   - CMD_ADD_UPDATE_CONTACT (9) setting contact flag 0x10 (remote CLI): it
//     would let that contact run the same CLI over the radio, past any bridge.
//
// Every other frame passes, including commands this package doesn't know.
// The CLI check matches one command, after normalizing the text the way the
// firmware does. That's a deliberate, narrow block list; a fork could add
// another command that reveals a secret.
//
// From the node, a RESP_CODE_PRIVATE_KEY (14) frame is dropped as a second
// layer, in case a refused export reaches the node by another path.
//
// The bridge enforces this on the host's machine, and the relay enforces it
// again, so neither a relay mistake nor an outdated bridge lets these through.
// Nothing here logs or returns frame contents: reasons are fixed strings.
//
// Layouts follow examples/companion_radio/MyMesh.cpp in meshcore-dev/MeshCore
// and src/MyMesh.cpp in ALLFATHER-BV/wadamesh (checked 2026-11-11).
package nodepolicy

import (
	"bytes"
	"strings"
)

// Companion commands and replies this package recognizes.
const (
	CmdAddUpdateContact byte = 9
	CmdExportPrivateKey byte = 23
	CmdImportPrivateKey byte = 24
	CmdFactoryReset     byte = 51
	CmdRunCLICommand    byte = 66

	RespPrivateKey byte = 14
	// RespDisabled is the firmware's own answer to a command it was built
	// without. A refused command is answered with it, so companion apps show
	// "not available" rather than waiting for a timeout.
	RespDisabled byte = 15

	// ContactFlagRemoteCLI lets a contact run CLI commands on this node over
	// the radio (ContactInfo::isRemoteCLIAllowed).
	ContactFlagRemoteCLI byte = 0x10
	// contactFlagsOffset is the flags byte in CMD_ADD_UPDATE_CONTACT:
	// [cmd][pubkey 32][type][flags]...
	contactFlagsOffset = 1 + 32 + 1
)

// Reasons a frame is refused. They're safe to log: never frame contents.
const (
	ReasonExportPrivateKey = "export_private_key"
	ReasonImportPrivateKey = "import_private_key"
	ReasonFactoryReset     = "factory_reset"
	ReasonWiFiPassword     = "cli_wifi_password"
	ReasonContactRemoteCLI = "contact_remote_cli"
	ReasonPrivateKeyReply  = "private_key_reply"
)

// ToNode reports whether a frame from a remote session may reach the node.
// When it may not, reason says why (one of the Reason constants).
func ToNode(frame []byte) (reason string, ok bool) {
	if len(frame) == 0 {
		return "", true
	}
	switch frame[0] {
	case CmdExportPrivateKey:
		return ReasonExportPrivateKey, false
	case CmdImportPrivateKey:
		return ReasonImportPrivateKey, false
	case CmdFactoryReset:
		return ReasonFactoryReset, false
	case CmdRunCLICommand:
		if isWiFiPasswordRead(frame[1:]) {
			return ReasonWiFiPassword, false
		}
	case CmdAddUpdateContact:
		// The firmware replaces a contact's flags with this byte, so an
		// update that sets remote CLI is refused rather than rewritten:
		// stripping the bit would silently revoke one the owner set locally.
		if len(frame) > contactFlagsOffset && frame[contactFlagsOffset]&ContactFlagRemoteCLI != 0 {
			return ReasonContactRemoteCLI, false
		}
	}
	return "", true
}

// FromNode reports whether a frame from the node may reach a remote session.
func FromNode(frame []byte) (reason string, ok bool) {
	if len(frame) > 0 && frame[0] == RespPrivateKey {
		return ReasonPrivateKeyReply, false
	}
	return "", true
}

// Refusal is the frame a remote session gets instead of the node's reply to
// a refused command.
func Refusal() []byte { return []byte{RespDisabled} }

// isWiFiPasswordRead reports whether CLI text would make the firmware return
// the WiFi password. The firmware reads up to the first NUL, skips leading
// spaces, then drops an optional 3-character "xx|" prefix, and compares
// exactly. This normalizes more loosely than that (any whitespace, case,
// repeated prefixes, trailing text), so it refuses everything the firmware
// would answer and a little more; refusing a harmless variant costs nothing.
func isWiFiPasswordRead(text []byte) bool {
	if i := bytes.IndexByte(text, 0); i >= 0 {
		text = text[:i]
	}
	s := strings.TrimSpace(string(text))
	for len(s) >= 3 && s[2] == '|' {
		s = strings.TrimSpace(s[3:])
	}
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	return strings.HasPrefix(s, "get wifi.pwd")
}
