package nodepolicy

import (
	"bytes"
	"testing"
)

func cli(text string) []byte { return append([]byte{CmdRunCLICommand}, text...) }

// raw is CMD_SEND_RAW_DATA with path_len and then the payload.
func raw(pathLen byte, payload string) []byte {
	return append([]byte{CmdSendRawData, pathLen}, payload...)
}

// txt is CMD_SEND_TXT_MSG to a 6-byte recipient prefix.
func txt(recipient, text string) []byte {
	f := []byte{CmdSendTxtMsg, 0, 0, 1, 2, 3, 4}
	f = append(f, recipient...)
	return append(f, text...)
}

func contact(flags byte) []byte {
	f := make([]byte, 1+32+2+1+64+32+4)
	f[0] = CmdAddUpdateContact
	f[1+32] = 1 // type: companion
	f[contactFlagsOffset] = flags
	return f
}

func TestToNode(t *testing.T) {
	cases := []struct {
		name   string
		frame  []byte
		reason string // "" means allowed
	}{
		{"empty", nil, ""},
		{"app start", []byte{1, 3, 0}, ""},
		{"device query", []byte{22, 3}, ""},
		{"export contact", append([]byte{17}, make([]byte, 32)...), ""},
		{"unknown opcode", []byte{200, 1, 2}, ""},
		{"export private key", []byte{CmdExportPrivateKey}, ReasonExportPrivateKey},
		{"export private key with junk", []byte{CmdExportPrivateKey, 9, 9}, ReasonExportPrivateKey},
		{"import private key", append([]byte{CmdImportPrivateKey}, make([]byte, 64)...), ReasonImportPrivateKey},
		{"factory reset", []byte{CmdFactoryReset}, ReasonFactoryReset},

		{"cli wifi password", cli("get wifi.pwd"), ReasonWiFiPassword},
		{"cli leading spaces", cli("   get wifi.pwd"), ReasonWiFiPassword},
		{"cli prefix", cli("ab|get wifi.pwd"), ReasonWiFiPassword},
		{"cli spaces then prefix", cli("  7x|get wifi.pwd"), ReasonWiFiPassword},
		{"cli repeated prefix", cli("ab|cd|get wifi.pwd"), ReasonWiFiPassword},
		{"cli NUL then junk", append(cli("get wifi.pwd"), 0, 'x'), ReasonWiFiPassword},
		{"cli case and tabs", cli("GET\twifi.PWD"), ReasonWiFiPassword},
		{"cli double space", cli("get  wifi.pwd"), ReasonWiFiPassword},
		{"cli trailing text", cli("get wifi.pwd please"), ReasonWiFiPassword},
		{"cli whitespace as the prefix", cli("\t\t|get wifi.pwd"), ReasonWiFiPassword},
		{"cli space and newlines as the prefix", cli(" \n\r|get wifi.pwd"), ReasonWiFiPassword},
		{"cli vertical tab prefix", cli("\v\f|get wifi.pwd"), ReasonWiFiPassword},
		{"cli NBSP prefix", cli("\u00a0|get wifi.pwd"), ReasonWiFiPassword},
		{"cli NEL prefix", cli("\u0085|get wifi.pwd"), ReasonWiFiPassword},
		{"cli wifi status", cli("get wifi.status"), ""},
		{"cli wifi ssid", cli("get wifi.ssid"), ""},
		{"cli set wifi password", cli("set wifi.pwd hunter2"), ""},
		{"cli name", cli("get name"), ""},
		{"cli empty", cli(""), ""},
		{"cli NUL first", append(cli(""), 0, 'g'), ""},

		{"contact without remote cli", contact(0x01), ""},
		{"contact with remote cli", contact(ContactFlagRemoteCLI), ReasonContactRemoteCLI},
		{"contact with remote cli and others", contact(0xff), ReasonContactRemoteCLI},
		{"short contact frame", []byte{CmdAddUpdateContact, 1, 2}, ""},

		{"raw data ota url", raw(0, "ota url https://example.invalid/fw.bin"), ReasonFirmwareUpdate},
		{"raw data ota spaced and cased", raw(0, " \tOTA start"), ReasonFirmwareUpdate},
		{"raw data ota bare", raw(0, "ota"), ReasonFirmwareUpdate},
		{"raw data with a path", raw(1, "\x42ota url x"), ""},
		{"raw data other payload", raw(0, "hello"), ""},
		{"raw data short", []byte{CmdSendRawData}, ""},
		{"meshcomod ota", txt("MESHCM", "ota url https://example.invalid/fw.bin"), ReasonFirmwareUpdate},
		{"meshcomod other command", txt("MESHCM", "wifi status"), ""},
		{"text to a contact saying ota", txt("ABCDEF", "ota url x"), ""},
		{"text short", []byte{CmdSendTxtMsg, 0, 0}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, ok := ToNode(c.frame)
			if ok != (c.reason == "") || reason != c.reason {
				t.Fatalf("ToNode = (%q, %v), want reason %q", reason, ok, c.reason)
			}
		})
	}
}

func TestFromNode(t *testing.T) {
	if reason, ok := FromNode(append([]byte{RespPrivateKey}, make([]byte, 64)...)); ok || reason != ReasonPrivateKeyReply {
		t.Fatalf("private key reply passed: (%q, %v)", reason, ok)
	}
	for _, f := range [][]byte{nil, {0}, {5, 1, 2}, {RespDisabled}, {0x80, 1}} {
		if _, ok := FromNode(f); !ok {
			t.Fatalf("frame % x refused", f)
		}
	}
}

func TestRefusalIsTheFirmwaresDisabledReply(t *testing.T) {
	if !bytes.Equal(Refusal(), []byte{15}) {
		t.Fatalf("Refusal() = % x", Refusal())
	}
}

// firmwareReturnsPassword models MyMesh::handleCommand in upstream MeshCore
// and wadamesh for CMD_RUN_CLI_COMMAND: the text ends at the first NUL,
// leading spaces are skipped, a 3-byte "xx|" prefix is dropped when the text
// is longer than 4 bytes, and the rest is compared exactly.
func firmwareReturnsPassword(text []byte) bool {
	if i := bytes.IndexByte(text, 0); i >= 0 {
		text = text[:i]
	}
	s := string(bytes.TrimLeft(text, " "))
	if len(s) > 4 && s[2] == '|' {
		s = s[3:]
	}
	return s == "get wifi.pwd"
}

// Every 2-byte prefix the firmware would strip, with and without leading
// spaces, is refused.
func TestEveryPrefixIsRefused(t *testing.T) {
	for a := 0; a < 256; a++ {
		for b := 0; b < 256; b++ {
			for _, lead := range []string{"", " ", "   "} {
				text := []byte(lead + string([]byte{byte(a), byte(b)}) + "|get wifi.pwd")
				if !firmwareReturnsPassword(text) {
					continue
				}
				if _, ok := ToNode(cli(string(text))); ok {
					t.Fatalf("prefix % x passed", text[:len(lead)+3])
				}
			}
		}
	}
}

// Anything the firmware model would answer with the password is refused.
func FuzzCLINeverLeaksWiFiPassword(f *testing.F) {
	for _, s := range []string{"get wifi.pwd", "  ab|get wifi.pwd", "\t\t|get wifi.pwd", "get wifi.pwd\x00x", "x"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, text []byte) {
		if firmwareReturnsPassword(text) {
			if _, ok := ToNode(append([]byte{CmdRunCLICommand}, text...)); ok {
				t.Fatalf("% x passed", text)
			}
		}
	})
}
