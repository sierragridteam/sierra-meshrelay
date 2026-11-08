// Command signrelease signs a release's SHA256SUMS for meshbridge's
// auto-update (see internal/update). The release workflow runs it with the
// signing key's seed (base64) in $MESHBRIDGE_RELEASE_SIGNING_KEY:
//
//	go run ./cmd/signrelease v0.7.0 dist/SHA256SUMS   # writes dist/SHA256SUMS.sig
//
// It then checks the signature against the key built into meshbridge, so a
// release can't go out signed with a key bridges won't trust.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/sierragridteam/sierra-meshrelay/internal/update"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "signrelease:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 3 {
		return fmt.Errorf("usage: signrelease <version> <path/to/SHA256SUMS>")
	}
	version, path := os.Args[1], os.Args[2]
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("MESHBRIDGE_RELEASE_SIGNING_KEY")))
	if err != nil || len(seed) != ed25519.SeedSize {
		return fmt.Errorf("$MESHBRIDGE_RELEASE_SIGNING_KEY must be a base64 ed25519 seed")
	}
	sums, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sig := update.Sign(ed25519.NewKeyFromSeed(seed), version, sums)
	if err := update.Verify(update.TrustedKeys(), version, sums, sig); err != nil {
		return fmt.Errorf("the signing key isn't the one meshbridge trusts: %w", err)
	}
	if err := os.WriteFile(path+".sig", []byte(sig+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("signed %s for %s\n", path, version)
	return nil
}
