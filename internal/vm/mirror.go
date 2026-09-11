package vm

import (
	"fmt"
	"io"
	"strings"
)

// This file points a node's package manager at a mirror the operator runs.
//
// It is the other half of the air-gap answer, and for most organisations it is
// the half that matters. A site with no internet almost always already has an
// internal mirror — Artifactory, Nexus, Satellite, a plain reverse proxy — and
// the useful thing k3helper can do is point at it, not replace it. Lending a
// node the operator's own connection (see viaproxy.go) is for sites that have
// no mirror either.

// AptMirror is a distribution mirror, e.g. https://nexus.corp/repository/ubuntu.
//
// The substitution is on the archive URL, keeping the suite and component
// after it, because that is how a mirror is meant to be a drop-in: the paths
// below the root are identical to the upstream's.
type AptMirror struct {
	// URL replaces the distribution archive.
	URL string
	// K8sRepo replaces the Kubernetes package repository, which is a separate
	// service from the distribution's and is mirrored separately. Empty leaves
	// pkgs.k8s.io alone — a site may mirror one and not the other.
	K8sRepo string
}

func (m AptMirror) empty() bool { return m.URL == "" && m.K8sRepo == "" }

// aptMirrorScript rewrites the node's sources to point at the mirror.
//
// Both source formats are handled: Ubuntu 24.04 ships deb822 files under
// /etc/apt/sources.list.d/*.sources, while older images and most third-party
// repositories use one-line entries. Missing one of them leaves half the
// sources pointing at an archive the node cannot reach, and `apt-get update`
// then fails on exactly the half that was missed.
//
// Every file is backed up before it is touched, so a mistyped mirror is one
// command away from being undone rather than a reinstall.
func aptMirrorScript(m AptMirror) string {
	if m.URL == "" {
		return ""
	}
	mirror := strings.TrimRight(m.URL, "/")
	return `
set -e
for f in /etc/apt/sources.list /etc/apt/sources.list.d/*.sources /etc/apt/sources.list.d/*.list; do
  [ -f "$f" ] || continue
  [ -f "$f.k3helper.bak" ] || cp -a "$f" "$f.k3helper.bak"
  # Matches the archive root of a Debian or Ubuntu mirror — including the
  # cloud images' own mirrors, which all end in /ubuntu or /ubuntu-ports — and
  # leaves everything after it alone, which is the suite and components.
  sed -i -E 's#https?://[A-Za-z0-9._~-]+(/[A-Za-z0-9._~-]+)*?/ubuntu-ports(/)?#` + mirror + `/#g; ' "$f"
  sed -i -E 's#https?://[A-Za-z0-9._~-]+(/[A-Za-z0-9._~-]+)*?/ubuntu(/)?#` + mirror + `/#g; ' "$f"
  sed -i -E 's#https?://[A-Za-z0-9._~-]+(/[A-Za-z0-9._~-]+)*?/debian(/)?#` + mirror + `/#g; ' "$f"
done
`
}

// applyAptMirror rewrites the sources on one node.
func applyAptMirror(t Target, m AptMirror, progress io.Writer) error {
	script := aptMirrorScript(m)
	if script == "" {
		return nil
	}
	if progress != nil {
		fmt.Fprintf(progress, "[%s] pointing apt at %s\n", t.Node.Host, m.URL)
	}
	out, code, err := t.Client.SudoRun(fmt.Sprintf("sh -c %s", shellQuote(script)))
	if err != nil || code != 0 {
		return fmt.Errorf("node %s: could not point apt at %s (exit %d): %s",
			t.Node.Host, m.URL, code, strings.TrimSpace(out))
	}
	// Prove it: a rewrite that silently matched nothing leaves the node
	// pointing at an archive it cannot reach, and the next apt-get update
	// fails for a reason that looks nothing like a mirror problem.
	check, _, _ := t.Client.Run(
		`grep -rhoE 'https?://[^ ]+' /etc/apt/sources.list /etc/apt/sources.list.d/ 2>/dev/null | head -20`)
	if check != "" && !strings.Contains(check, strings.TrimRight(m.URL, "/")) {
		return fmt.Errorf("node %s: the sources still do not mention %s after rewriting them:\n%s\n"+
			"Restore them with: cp <file>.k3helper.bak <file>", t.Node.Host, m.URL, strings.TrimSpace(check))
	}
	return nil
}
