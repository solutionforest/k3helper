// Package bundle assembles everything a node needs to install k3s without
// reaching the internet.
//
// The normal install is `curl -sfL https://get.k3s.io | sh -`, which needs
// three things the node cannot have in an air-gapped network: the channel
// service that resolves "stable" to a version, the GitHub release that holds
// the k3s binary, and the container registry every pod pulls its image from.
// A bundle is those first two fetched somewhere with a connection; the third
// is covered by the airgap image archive, which k3s imports into containerd on
// first start.
package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Files inside a bundle directory. The names are fixed so that `vm setup
// --bundle` can find them without being told, and so a bundle can be checked
// by eye.
const (
	BinaryName   = "k3s"
	ImagesName   = "k3s-airgap-images.tar.zst"
	InstallName  = "install.sh"
	ManifestName = "bundle.json"
)

// Manifest records what a bundle holds. It is written into the directory so a
// bundle carried on a USB stick still knows its own version and architecture —
// installing an arm64 k3s on an amd64 node otherwise fails as "cannot execute
// binary file", which is a long way from the cause.
type Manifest struct {
	Version   string    `json:"version"`
	Arch      string    `json:"arch"`
	Created   time.Time `json:"created"`
	BinarySHA string    `json:"binary_sha256"`
	ImagesSHA string    `json:"images_sha256"`
}

// Options control a fetch.
type Options struct {
	Version string // e.g. v1.31.2+k3s1; required, because a channel lookup is the thing being avoided
	Arch    string // amd64 or arm64
	Dir     string // where to write
	// Progress receives one line per step.
	Progress io.Writer
	// HTTPClient is overridden by tests.
	HTTPClient *http.Client
}

func (o Options) client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	// Generous: these are large files over a link that may be slow, and a
	// timeout that kills a 90%-complete 250MB download helps nobody.
	return &http.Client{Timeout: 30 * time.Minute}
}

// ReleaseBase is where the k3s release assets live. Overridden by tests.
var ReleaseBase = "https://github.com/k3s-io/k3s/releases/download"

// InstallScriptURL is the installer itself.
var InstallScriptURL = "https://get.k3s.io"

// binaryAsset is the release asset name for the k3s binary on an architecture.
// amd64 is the unsuffixed one, which is easy to get wrong in the other
// direction and produces a bundle that cannot run anywhere.
func binaryAsset(arch string) string {
	if arch == "amd64" {
		return "k3s"
	}
	return "k3s-" + arch
}

func imagesAsset(arch string) string {
	return fmt.Sprintf("k3s-airgap-images-%s.tar.zst", arch)
}

func checksumAsset(arch string) string {
	return fmt.Sprintf("sha256sum-%s.txt", arch)
}

// Fetch downloads a bundle into o.Dir.
func Fetch(o Options) (*Manifest, error) {
	if o.Version == "" {
		return nil, fmt.Errorf("a version is required (e.g. v1.31.2+k3s1) — " +
			"an air-gapped install cannot resolve a channel, which is the point of a bundle")
	}
	if o.Arch == "" {
		o.Arch = "amd64"
	}
	switch o.Arch {
	case "amd64", "arm64":
	default:
		return nil, fmt.Errorf("unsupported architecture %q: k3s publishes amd64 and arm64", o.Arch)
	}
	if o.Dir == "" {
		return nil, fmt.Errorf("an output directory is required")
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", o.Dir, err)
	}

	base := fmt.Sprintf("%s/%s", ReleaseBase, urlVersion(o.Version))

	// The checksum manifest first: downloading 250MB and only then finding out
	// there is nothing to check it against wastes the slow part.
	sums, err := fetchChecksums(o, base+"/"+checksumAsset(o.Arch))
	if err != nil {
		return nil, err
	}

	m := &Manifest{Version: o.Version, Arch: o.Arch, Created: time.Now().UTC()}

	binSum, err := download(o, base+"/"+binaryAsset(o.Arch),
		filepath.Join(o.Dir, BinaryName), 0o755, sums[binaryAsset(o.Arch)])
	if err != nil {
		return nil, err
	}
	m.BinarySHA = binSum

	imgSum, err := download(o, base+"/"+imagesAsset(o.Arch),
		filepath.Join(o.Dir, ImagesName), 0o644, sums[imagesAsset(o.Arch)])
	if err != nil {
		return nil, err
	}
	m.ImagesSHA = imgSum

	// The install script has no published checksum, so it is fetched without
	// one rather than pretending otherwise.
	if _, err := download(o, InstallScriptURL,
		filepath.Join(o.Dir, InstallName), 0o755, ""); err != nil {
		return nil, err
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(o.Dir, ManifestName), append(data, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("write manifest: %w", err)
	}
	progressf(o, "✓ bundle ready in %s (%s, %s)", o.Dir, o.Version, o.Arch)
	return m, nil
}

// urlVersion escapes the "+" in a k3s version, which is a real character in
// the tag and means "space" in a URL path if left alone.
func urlVersion(v string) string {
	return strings.ReplaceAll(v, "+", "%2B")
}

// Load reads the manifest of an existing bundle and checks the files are
// actually there, so `vm setup --bundle` fails at the front door rather than
// half way through an install on the first node.
func Load(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s is not a k3s bundle (no %s) — make one with `k3helper bundle k3s --version <ver> -o %s`",
				dir, ManifestName, dir)
		}
		return nil, err
	}
	m := &Manifest{}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ManifestName, err)
	}
	for _, f := range []string{BinaryName, ImagesName, InstallName} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return nil, fmt.Errorf("bundle %s is incomplete: %s is missing", dir, f)
		}
	}
	return m, nil
}

func fetchChecksums(o Options, url string) (map[string]string, error) {
	progressf(o, "fetching checksums...")
	resp, err := o.client().Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch checksums: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch checksums: %s returned %s — is %q a real k3s release?",
			url, resp.Status, o.Version)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	sums := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			sums[strings.TrimPrefix(f[1], "./")] = f[0]
		}
	}
	return sums, nil
}

// download fetches one asset, verifying it against want when there is one.
//
// The hash is computed while the bytes are written rather than by reading the
// file back: these are large, and a bundle built on a laptop should not need
// to read 250MB twice.
func download(o Options, url, dest string, mode os.FileMode, want string) (string, error) {
	progressf(o, "downloading %s...", filepath.Base(dest))
	resp, err := o.client().Get(url)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: %s", url, resp.Status)
	}

	tmp := dest + ".part"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", tmp, err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	closeErr := f.Close()
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return "", closeErr
	}

	got := hex.EncodeToString(h.Sum(nil))
	if want != "" && !strings.EqualFold(got, want) {
		os.Remove(tmp)
		return "", fmt.Errorf("checksum mismatch for %s: expected %s, got %s", filepath.Base(dest), want, got)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	if err := os.Chmod(dest, mode); err != nil {
		return "", err
	}
	progressf(o, "  %s  %s  %s", filepath.Base(dest), humanSize(n), shortSum(got))
	return got, nil
}

func progressf(o Options, format string, args ...any) {
	if o.Progress == nil {
		return
	}
	fmt.Fprintf(o.Progress, format+"\n", args...)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0fMB", float64(n)/float64(1<<20))
	default:
		return fmt.Sprintf("%dKB", n/1024)
	}
}

func shortSum(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
