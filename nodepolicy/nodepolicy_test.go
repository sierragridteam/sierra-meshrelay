package nodepolicy

import (
	"bytes"
	"testing"
)

func cli(text string) []byte { return append([]byte{CmdRunCLICommand}, text...) }

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
