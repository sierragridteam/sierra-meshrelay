package relay

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// Token prefixes make a leaked token recognizable (and greppable by secret
// scanners) and keep the two kinds from being confused at a glance.
const (
	BridgeTokenPrefix = "smb_"
	ClientTokenPrefix = "smc_"
)

// Role is a client token's authority. See docs/relay.md "Roles".
type Role string

const (
	// RoleAdmin can do everything: create gateways, issue and revoke tokens,
	// accept pubkey changes, preempt leases.
	RoleAdmin Role = "admin"
	// RoleOperator takes leases on any gateway and views status.
	RoleOperator Role = "operator"
	// RoleAgent takes leases (Elmer, meshcore-mcp, future pollers) and views
	// status.
	RoleAgent Role = "agent"
)

// ParseRole validates a role name.
func ParseRole(s string) (Role, error) {
	switch r := Role(strings.ToLower(strings.TrimSpace(s))); r {
	case RoleAdmin, RoleOperator, RoleAgent:
		return r, nil
	}
	return "", fmt.Errorf("unknown role %q (want admin, operator or agent)", s)
}

// gatewayNamePattern is the protocol's gateway name rule.
var gatewayNamePattern = regexp.MustCompile(`^[a-z0-9-]{3,40}$`)

// reservedNames are path segments the relay serves itself under /mesh/relay/,
// so a gateway can never shadow them.
var reservedNames = map[string]bool{"api": true, "healthz": true, "status": true, "static": true}

// ValidGatewayName reports whether name is a legal, unreserved gateway name.
func ValidGatewayName(name string) bool {
	return gatewayNamePattern.MatchString(name) && !reservedNames[name]
}

// clientNamePattern constrains a client name. It appears in audit rows and in
// the bridge's attach message, so keep it a plain slug.
var clientNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidClientName reports whether name is a legal client name.
func ValidClientName(name string) bool { return clientNamePattern.MatchString(name) }

// NormalizePubkey lowercases a hex pubkey and reports whether it is well formed.
func NormalizePubkey(s string) (string, bool) { return meshproto.NormalizePubkey(s) }

// MintToken returns a new opaque token: prefix + base64url(32 random bytes).
func MintToken(prefix string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// TokenHash is the only form of a token the relay stores. A SHA-256 of 256
// random bits cannot be reversed or usefully brute-forced, so no salt or KDF is
// needed (unlike a password).
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// randomID returns prefix + 16 hex characters, for session and lease ids.
func randomID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// bearerToken extracts the token from an Authorization header.
func bearerToken(header string) string {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}
