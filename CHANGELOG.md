# Changelog

Notable changes per release. The release workflow publishes the section
matching the tag it is building, so this file is the source of the release
notes on GitHub.

## v0.1.0

The first release that is useful on a machine you cannot SSH into, and the
first that verifies who it is talking to.

### Install

```bash
curl -fsSL https://raw.githubusercontent.com/solutionforest/k3helper/main/install.sh | sh
```

Or take a single binary — `k3helper-{linux,darwin}-{amd64,arm64}`, each listed
in `checksums.txt`.

### ⚠️ Behaviour change: SSH host keys are now verified

v0.0.1 accepted any host key on every connection. v0.1.0 verifies against
`~/.ssh/known_hosts` and **refuses to connect to a host it has not seen**,
because `vm setup` sends a cluster join token and pipes an install script into
a root shell over that connection.

An inventory that worked before will now stop on first contact. Three ways
through, best first:

| | Behaviour |
|---|---|
| `ssh-keyscan -H <host> >> ~/.ssh/known_hosts` | you check the fingerprint out of band |
| `--accept-new-host-key` | trust on first use, record it, still refuse if it later changes |
| `--insecure-host-key` | accept anything (also per node: `insecure_host_key: true`) |

A key that *changes* is refused in every mode but the last — that is the case
worth stopping for.

### Run k3helper on the node it manages

A node with `local: true` in `targets.yaml` executes through `/bin/sh` instead
of dialling SSH — no sshd, no key, no loopback connection. This is for hosts
reachable only through a provider's browser console, where there is no inbound
SSH to use. When already root it also skips `sudo`, which minimal console-only
images often do not ship.

`k3helper init --local` writes that file in one command. For a server with no
outbound internet at all, `make bundle` produces a chunked, paste-able bundle
whose installer refuses to proceed unless line count, byte size and SHA-256 all
match.

### New commands

- **`init`** — writes `targets.yaml` from flags, `--local`, or as an annotated
  template.
- **`ctx`** — lists the clusters in a targets file. One file can hold several
  under `clusters:`, selected with `--context` on any subcommand; the
  single-cluster format keeps working unchanged.

### deploy

- `--diff` shows what would change against live cluster state before applying.
  Pair with `--dry-run` to look without touching anything.

### doctor

Nine new signatures: TLS certificate expiry, embedded-etcd quorum, CoreDNS
outages, Services with no ready endpoints, evicted pods, container runtime
down, clock skew, image-cache bloat, and pods running but never ready.

Evidence that could **not** be gathered is now reported rather than quietly
narrowing the diagnosis, so "no issues detected" never silently means "did not
look". `--json` emits signature IDs for scripting.

### check

- `--strict` escalates warnings to a failing exit code. By default warnings are
  printed but do not fail: a swap warning should not break a pipeline the way a
  dead k3s does.
- Adapts per node to k3s (`k3s`/`k3s-agent`) or kubeadm (`kubelet` +
  `containerd`), detected from installed unit files, with the matching
  kubeconfig path. A node with neither is told to install one rather than to
  restart something that never existed.

### TUI

A k9s-style resource browser alongside the health dashboard: `:pods`, `:nodes`,
`:events`, `:ns <name>`, with logs, describe, and filtering. The status column
shows the container's reason, so `CrashLoopBackOff` reads as
`CrashLoopBackOff` rather than `Pending`; opening logs on a crashlooping pod
falls back to the previous instance, which is where the cause usually is.

### Fixes

- **The fetched kubeconfig now works.** `--kubeconfig` rewrites k3s's
  `127.0.0.1` to the node's real address instead of copying it verbatim, which
  pointed kubectl at your own loopback.
- **`vm setup` waits for every node in the targets file** to register *and* go
  Ready, not just for whichever registered so far — "cluster ready" could mean
  a server with no agents attached. Timeouts now say which half failed.
- Join tokens are shell-quoted before being piped into a root shell.
- `~` is expanded in targets-file key paths.

### Testing

`make fault-check-all` is the troubleshooter's exam: it injects each of eleven
faults, asserts `doctor` reports the signature that fault should produce, then
cleans up. Matching is on signature IDs from `doctor --json`, not display
titles, so rewording a finding cannot silently break the suite.

The sandbox gained a second driver — `docker` (privileged systemd containers)
alongside `orbstack` (Linux VMs) — selected automatically and overridable with
`SANDBOX_DRIVER`. The container driver is not yet proven end to end and its CI
job runs on `workflow_dispatch` only, rather than blocking every push on an
unverified result.

## v0.0.1

First release: VM setup over SSH, YAML verify and generate, deploy, host and
cluster checks, doctor, and the TUI dashboard.
