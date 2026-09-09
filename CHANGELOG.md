# Changelog

Notable changes per release. The release workflow publishes the section
matching the tag it is building, so this file is the source of the release
notes on GitHub.

## v0.2.0

Bootstrapping now covers both distributions k3helper can already diagnose, and
a control plane can be more than one machine.

### Install

```bash
curl -fsSL https://raw.githubusercontent.com/solutionforest/k3helper/main/install.sh | sh
```

Or take a single binary — `k3helper-{linux,darwin}-{amd64,arm64}`, each listed
in `checksums.txt`. No breaking changes from v0.1.0.

### HA control plane

Give the targets file more than one `role: server` node and `vm setup` builds
an embedded-etcd control plane: the first is installed with `--cluster-init`,
the rest join with `--server`, **one at a time** — etcd learners join
sequentially and a batch of them can cost quorum. A single server keeps k3s's
default sqlite datastore.

An even number of servers is refused with an explanation rather than quietly
built: four tolerate the same single failure as three, and lose quorum at two.

### vm setup for kubeadm

```bash
k3helper vm setup -t targets.yaml --distro kubeadm --k8s-version v1.31 --cni flannel
```

Prepares each host — containerd with the systemd cgroup driver, kernel modules,
sysctls, the Kubernetes apt repository, and the packages kubeadm needs and does
not install itself (`conntrack`, `socat`, `ethtool`) — then runs `kubeadm init`,
installs a CNI, and joins the workers.

`--no-conntrack-tuning` is needed where `/proc/sys/net/netfilter` is read-only
or capped below what kube-proxy wants: nested VMs, containers, some managed
images. kube-proxy raises that sysctl at startup and **exits** if the write is
refused, which stops Service routing, which then stops the CNI reaching the API
service. What you see is a crashlooping CNI, nowhere near the cause.

kubeadm HA is not supported: joining more control-plane nodes needs
`--upload-certs` and an endpoint in front of the API servers. `vm setup` says
so rather than building half of it.

### doctor

- `--watch 30s` re-runs on an interval, redrawing in full only when the verdict
  changes so a long watch does not bury the moment things went wrong. It
  reconnects each pass: a node going away is one of the things being watched
  for, and a held-open connection would keep reporting the state it last saw.
- **etcd quorum is now assessed without a working API server.** Losing quorum
  is exactly what stops the API server answering, so asking it about its own
  members failed in the case that mattered most. It now falls back to the host
  layer — how many control-plane nodes have a running service, counting
  unreachable ones against quorum too.
- A NotReady node and a stopped service on the same machine no longer produce
  two findings for one fault. Cluster evidence names nodes as Kubernetes does
  (`sandbox-agent2`); host evidence uses the targets file's name (`agent2`).
  Nothing bridged the two.

### TUI

- `:ports` — a port-forward manager. `f` on a pod starts `kubectl port-forward`
  on the cluster node **and** an SSH tunnel to it, because kubectl binds on the
  node it runs on; without the tunnel the port is open there and not on your
  machine. The list shows the local address to connect to, and `x` tears one
  down — remote process and tunnel both.
- `:logs` — tails every pod matching the current filter and labels each line
  with the pod that wrote it. Changing the filter re-tails.

### Testing

The sandbox has a second driver: three privileged systemd containers, for hosts
that cannot nest virtualisation. It provisions correctly and k3s reaches Ready,
but **pod networking does not work there** — pods get addresses from the
flannel range that nothing can reach, not even their own node, so readiness
probes fail. Mounting `/lib/modules` removed one real blocker; what remains is
container-in-container CNI. The CI E2E job stays `workflow_dispatch`-only and
labelled unproven rather than reporting a pass nobody has seen.

Verified for this release against the sandbox: a 3-server embedded-etcd cluster
built by this code (stopping one member reports etcd degraded under the stopped
service; stopping a second takes the API server down and quorum loss then leads
at 95% from host evidence alone), a kubeadm v1.31 cluster with every pod
Running, an HTTP request reaching an nginx pod through the port-forward tunnel,
`make e2e` at 66 passed / 0 failed, and `make fault-check-all` at 10 / 0.

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
