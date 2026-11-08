// Package update stages and applies signed meshbridge releases.
//
// Trust is anchored in an ed25519 key: the release workflow signs
// "meshbridge release <version>\n" followed by the release's SHA256SUMS, and
// publishes the signature as SHA256SUMS.sig (base64). A bridge accepts a
// release only if that signature verifies against a key built into it and
// the archive's SHA-256 is in the signed SHA256SUMS. Neither GitHub nor the
// relay (which only says which version to install) can push code a bridge
// will run.
//
// Updating has two halves, because the running bridge can't replace its own
// binary (systemd's sandbox and the Windows service account both keep it
// read-only, on purpose):
//
//   - Stage (the bridge): download and verify into <config dir>/update/.
//   - Apply (root on Linux, via ExecStartPre; SYSTEM on Windows, via a
//     scheduled task): verify again, check the new binary runs, and swap it
//     in.
package update

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// releaseKey is the release signing key's public half (base64).
const releaseKey = "NEx/iDcd5IM0UvKFJUc8BM0n4N1LzmU2RZ3TM0SdLPE="

// extraKey adds a trusted key at build time, for tests only:
// -ldflags "-X github.com/sierragridteam/sierra-meshrelay/internal/update.extraKey=…".
var extraKey string

// TrustedKeys are the keys a release signature may verify against.
func TrustedKeys() []ed25519.PublicKey {
	var out []ed25519.PublicKey
	for _, k := range []string{releaseKey, extraKey} {
		if b, err := base64.StdEncoding.DecodeString(k); err == nil && len(b) == ed25519.PublicKeySize {
			out = append(out, ed25519.PublicKey(b))
		}
	}
	return out
}

// Files in a release and in the staging directory.
const (
	SumsFile    = "SHA256SUMS"
	SigFile     = "SHA256SUMS.sig"
	versionFile = "version"
	readyFile   = "ready"
	StageDir    = "update"
)

// signedMessage is what a release signature covers.
func signedMessage(version string, sums []byte) []byte {
	return append([]byte("meshbridge release "+version+"\n"), sums...)
}

// Sign signs a release's SHA256SUMS (for the release workflow).
func Sign(key ed25519.PrivateKey, version string, sums []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(key, signedMessage(version, sums)))
}

// Verify checks a release signature against keys.
func Verify(keys []ed25519.PublicKey, version string, sums []byte, sig string) error {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sig))
	if err != nil {
		return errors.New("release signature isn't base64")
	}
	msg := signedMessage(version, sums)
	for _, k := range keys {
		if ed25519.Verify(k, msg, raw) {
			return nil
		}
	}
	return errors.New("release signature doesn't verify against a trusted key")
}

// AssetName is this build's release archive name.
func AssetName() string { return assetName(runtime.GOOS, runtime.GOARCH, goarm()) }

func assetName(goos, goarch, arm string) string {
	name := "meshbridge_" + goos + "_" + goarch
	if goarch == "arm" && arm != "" {
		name += "v" + strings.TrimSuffix(arm, ",softfloat")
	}
	if goos == "windows" {
		return name + ".zip"
	}
	return name + ".tar.gz"
}

func goarm() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "GOARM" {
				return s.Value
			}
		}
	}
	return ""
}

// BinaryName is the executable's name inside an archive.
func BinaryName() string {
	if runtime.GOOS == "windows" {
		return "meshbridge.exe"
	}
	return "meshbridge"
}

// Newer reports whether version a is newer than b (vX.Y.Z; anything that
// doesn't parse, such as "dev", is never newer and never older).
func Newer(a, b string) bool {
	pa, oka := parse(a)
	pb, okb := parse(b)
	if !oka || !okb {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		if i == 2 {
			p, _, _ = strings.Cut(p, "-") // v1.2.3-rc1 counts as v1.2.3
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// sumFor finds an asset's SHA-256 in a SHA256SUMS file.
func sumFor(sums []byte, asset string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			return strings.ToLower(f[0]), true
		}
	}
	return "", false
}

// Stager downloads releases into dir/update.
type Stager struct {
	Dir  string // the config directory
	HTTP *http.Client
	Keys []ed25519.PublicKey // default TrustedKeys()
}

const maxDownload = 64 << 20

func (s *Stager) get(ctx context.Context, url string) ([]byte, error) {
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if len(b) > maxDownload {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", url, maxDownload)
	}
	return b, err
}

// Stage downloads version from baseURL (the release's download directory),
// verifies it and leaves it in dir/update, replacing anything staged before.
func (s *Stager) Stage(ctx context.Context, baseURL, version string) error {
	keys := s.Keys
	if keys == nil {
		keys = TrustedKeys()
	}
	if len(keys) == 0 {
		return errors.New("this build has no release signing key")
	}
	baseURL = strings.TrimRight(baseURL, "/")
	sums, err := s.get(ctx, baseURL+"/"+SumsFile)
	if err != nil {
		return err
	}
	sig, err := s.get(ctx, baseURL+"/"+SigFile)
	if err != nil {
		return err
	}
	if err := Verify(keys, version, sums, string(sig)); err != nil {
		return err
	}
	asset := AssetName()
	want, ok := sumFor(sums, asset)
	if !ok {
		return fmt.Errorf("release %s has no %s", version, asset)
	}
	archive, err := s.get(ctx, baseURL+"/"+asset)
	if err != nil {
		return err
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s doesn't match its checksum", asset)
	}
	tmp := filepath.Join(s.Dir, StageDir+".tmp")
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	for name, data := range map[string][]byte{SumsFile: sums, SigFile: sig, asset: archive, versionFile: []byte(version)} {
		if err := os.WriteFile(filepath.Join(tmp, name), data, 0o600); err != nil {
			return err
		}
	}
	final := filepath.Join(s.Dir, StageDir)
	_ = os.RemoveAll(final)
	return os.Rename(tmp, final)
}

// MarkReady says the staged update may be applied now (the bridge is idle).
func MarkReady(dir string) error {
	return os.WriteFile(filepath.Join(dir, StageDir, readyFile), nil, 0o600)
}

// Staged returns the staged version, if any, and whether it's marked ready.
func Staged(dir string) (version string, ready bool) {
	b, err := os.ReadFile(filepath.Join(dir, StageDir, versionFile))
	if err != nil {
		return "", false
	}
	_, err = os.Stat(filepath.Join(dir, StageDir, readyFile))
	return strings.TrimSpace(string(b)), err == nil
}

// Discard removes a staged update.
func Discard(dir string) error { return os.RemoveAll(filepath.Join(dir, StageDir)) }

// Apply verifies the update staged in dir and installs its binary at
// binary, if it is newer than current. It always removes the staged files,
// so a bad update isn't retried forever. It returns the installed version,
// or "" if there was nothing (newer) to apply.
func Apply(dir, binary, current string, keys []ed25519.PublicKey) (string, error) {
	version, _ := Staged(dir)
	if version == "" {
		return "", nil
	}
	defer Discard(dir) //nolint:errcheck // best effort
	if keys == nil {
		keys = TrustedKeys()
	}
	if !Newer(version, current) {
		return "", nil
	}
	stage := filepath.Join(dir, StageDir)
	sums, err := os.ReadFile(filepath.Join(stage, SumsFile))
	if err != nil {
		return "", err
	}
	sig, err := os.ReadFile(filepath.Join(stage, SigFile))
	if err != nil {
		return "", err
	}
	if err := Verify(keys, version, sums, string(sig)); err != nil {
		return "", err
	}
	asset := AssetName()
	want, ok := sumFor(sums, asset)
	if !ok {
		return "", fmt.Errorf("staged release has no %s", asset)
	}
	archive, err := os.ReadFile(filepath.Join(stage, asset))
	if err != nil {
		return "", err
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != want {
		return "", fmt.Errorf("staged %s doesn't match its checksum", asset)
	}
	exe, err := extract(archive, asset)
	if err != nil {
		return "", err
	}
	tmp := binary + ".new"
	if err := os.WriteFile(tmp, exe, 0o755); err != nil {
		return "", err
	}
	// The new binary must at least run and say it's the version we expect.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, tmp, "version").CombinedOutput()
	if err != nil || !strings.Contains(string(out), version) {
		os.Remove(tmp)
		return "", fmt.Errorf("the new binary didn't run (%v): %s", err, firstLine(out))
	}
	if runtime.GOOS == "windows" {
		// Windows can't rename over a file that may be mapped; move the old
		// one aside first.
		old := binary + ".old"
		_ = os.Remove(old)
		if err := os.Rename(binary, old); err != nil && !errors.Is(err, os.ErrNotExist) {
			os.Remove(tmp)
			return "", err
		}
	}
	if err := os.Rename(tmp, binary); err != nil {
		return "", err
	}
	return version, nil
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return s
}

// extract returns the meshbridge executable from a release archive.
func extract(archive []byte, asset string) ([]byte, error) {
	want := BinaryName()
	if strings.HasSuffix(asset, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == want {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(io.LimitReader(rc, maxDownload))
			}
		}
		return nil, fmt.Errorf("%s isn't in %s", want, asset)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s isn't in %s", want, asset)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == want {
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
}
