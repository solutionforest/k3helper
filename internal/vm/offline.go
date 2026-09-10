package vm

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/solutionforest/k3helper/internal/bundle"
)

// This file installs k3s on a node that cannot reach the internet.
//
// The online install is one pipe: `curl -sfL https://get.k3s.io | sh -`. That
// needs three things an air-gapped node does not have — update.k3s.io to
// resolve a channel, the GitHub release to fetch the binary, and a registry
// for every image a pod pulls. The offline install supplies all three from a
// bundle: the binary is placed where the installer expects to find it, the
// airgap image archive is dropped where k3s imports it into containerd on
// first start, and INSTALL_K3S_SKIP_DOWNLOAD tells the installer not to reach
// for any of it.

// Uploader is a Host that can also receive files. Split from Host because the
// online path does not need it and its tests would otherwise have to grow
// methods they never call.
type Uploader interface {
	Host
	WriteFileFrom(remotePath string, r io.Reader, mode os.FileMode, progress func(int64)) error
}

// Paths the k3s installer reads when INSTALL_K3S_SKIP_DOWNLOAD is set.
const (
	remoteBinary   = "/usr/local/bin/k3s"
	remoteImageDir = "/var/lib/rancher/k3s/agent/images"
	remoteStage    = "/tmp/k3helper-k3s-bundle"
)

// stageBundle puts a bundle's contents on one node.
//
// Files land in /tmp first and are moved into place with sudo, because the
// login user can rarely write to /usr/local/bin or /var/lib/rancher directly —
// and a partially written k3s binary at the real path is worse than none.
func stageBundle(t Target, dir string, m *bundle.Manifest, progress io.Writer) error {
	up, ok := t.Client.(Uploader)
	if !ok {
		return fmt.Errorf("node %s: this connection cannot upload files, so an offline install is not possible", t.Node.Host)
	}
	say := func(format string, a ...any) {
		if progress != nil {
			fmt.Fprintf(progress, format+"\n", a...)
		}
	}

	if _, code, err := t.Client.Run(fmt.Sprintf("mkdir -p '%s'", remoteStage)); err != nil || code != 0 {
		return fmt.Errorf("node %s: could not create a staging directory", t.Node.Host)
	}

	for _, f := range []struct {
		name string
		mode os.FileMode
	}{
		{bundle.BinaryName, 0o755},
		{bundle.ImagesName, 0o644},
		{bundle.InstallName, 0o755},
	} {
		local := filepath.Join(dir, f.name)
		info, err := os.Stat(local)
		if err != nil {
			return fmt.Errorf("bundle file %s: %w", local, err)
		}
		src, err := os.Open(local)
		if err != nil {
			return err
		}
		size := info.Size()
		say("[%s] uploading %s (%s)...", t.Node.Host, f.name, humanSize(size))
		err = up.WriteFileFrom(remoteStage+"/"+f.name, src, f.mode, func(sent int64) {
			if size > 0 {
				say("[%s]   %s %3d%%", t.Node.Host, f.name, sent*100/size)
			}
		})
		src.Close()
		if err != nil {
			return fmt.Errorf("node %s: upload %s: %w", t.Node.Host, f.name, err)
		}
	}

	// Put everything where the installer looks. The image archive keeps its
	// architecture-qualified name: k3s does not care what the file is called,
	// but an operator looking at the directory later does.
	place := fmt.Sprintf(
		`install -m 0755 '%s/%s' '%s' && mkdir -p '%s' && install -m 0644 '%s/%s' '%s/k3s-airgap-images-%s.tar.zst'`,
		remoteStage, bundle.BinaryName, remoteBinary,
		remoteImageDir,
		remoteStage, bundle.ImagesName, remoteImageDir, m.Arch,
	)
	if out, code, err := t.Client.SudoRun(place); err != nil || code != 0 {
		return fmt.Errorf("node %s: could not place the bundle (exit %d): %s", t.Node.Host, code, out)
	}
	return nil
}

// offlineInstallCmd renders the install command for a staged bundle.
//
// INSTALL_K3S_SKIP_DOWNLOAD makes the installer use the binary already at
// /usr/local/bin/k3s and skip both the channel lookup and the release
// download. Without it the script fetches regardless of what is on disk, which
// on an air-gapped node is a TLS error and a puzzled operator.
func offlineInstallCmd(sudoPrefix, env, role, args string) string {
	return fmt.Sprintf(
		`%sINSTALL_K3S_SKIP_DOWNLOAD=true INSTALL_K3S_BIN_DIR=/usr/local/bin %ssh '%s/%s' %s%s`,
		sudoPrefix, env, remoteStage, bundle.InstallName, role, args,
	)
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
