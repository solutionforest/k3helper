package kyaml

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/solutionforest/k3helper/internal/kube"
)

func randomSuffix() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate temp name: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Target is a machine that can run kubectl and receive files — satisfied by
// *ssh.Client. Mirrors deploy.Executor; kept local to avoid an import cycle.
type Target interface {
	Run(cmd string) (string, int, error)
	WriteFile(remotePath string, data []byte, mode os.FileMode) error
	RemoveFile(remotePath string) error
}

// KubectlBase is the kubectl invocation used on a k3s server node.
//
// Deprecated: prefer kube.Builder, which detects the node's distribution.
// (kube.Cmd is k3s-only too.) Kept because the integration tests build cleanup
// commands with it against the k3s sandbox.
const KubectlBase = `sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml`

// VerifyLive is validation layer 3 against a real API server: it uploads the
// manifest to the target and runs `kubectl apply --dry-run=server`, which
// catches everything the offline rules cannot (unknown fields, admission
// webhooks, CRD schemas, immutability).
//
// The returned Result carries the offline verdict merged with the server's.
func VerifyLive(t Target, data []byte, name string) (*Result, error) {
	res := Verify(data)

	remote, err := RemotePath(name)
	if err != nil {
		return res, err
	}
	if err := t.WriteFile(remote, data, 0o600); err != nil {
		return res, fmt.Errorf("upload manifest: %w", err)
	}
	defer t.RemoveFile(remote)

	kubectl := kube.Builder(t)
	out, code, err := t.Run(kubectl(fmt.Sprintf(`apply --dry-run=server -f '%s'`, remote)) + " 2>&1")
	if err != nil {
		return res, fmt.Errorf("kubectl: %w", err)
	}
	if code != 0 {
		res.OK = false
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			res.Issues = append(res.Issues, Issue{Field: "server", Message: line})
		}
	}
	return res, nil
}

// RemotePath builds a collision-resistant /tmp path from a manifest name,
// keeping only characters that are safe inside a single-quoted shell word.
func RemotePath(name string) (string, error) {
	base := name
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	base = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '-', r == '_':
			return r
		}
		return '-'
	}, base)
	if base == "" || strings.Trim(base, ".-_") == "" {
		base = "manifest.yaml"
	}
	if len(base) > 64 {
		base = base[len(base)-64:]
	}
	suffix, err := randomSuffix()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/tmp/k3helper-%s-%s", suffix, base), nil
}
