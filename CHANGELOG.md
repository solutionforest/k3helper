# Changelog

Notable changes per release. The release workflow publishes the section
matching the tag it is building, so this file is the source of the release
notes on GitHub.

## Unreleased

Live testing against real DigitalOcean VMs, and the four bugs it found.

### Added

- **`k3helper bundle k3s`** builds an offline install bundle — the k3s binary,
  the airgap image archive and the installer — verified against the release
  sha256 manifest. `k3helper vm setup --bundle <dir>` installs from it with
  `INSTALL_K3S_SKIP_DOWNLOAD`, so the nodes need no internet at all. Proven on
  two DigitalOcean droplets with egress blocked at the provider firewall:
  `github=000, get.k3s.io=000`, cluster Ready.
- **`--k3s-version`** pins an exact release and skips the update.k3s.io channel
  lookup. During testing that service served a Traefik default certificate from
  every one of its addresses, which breaks `curl -sfL https://get.k3s.io | sh -`
  everywhere; a pinned version fetches from the GitHub release instead.
- **`--join-address`** overrides the address other nodes dial to reach the
  first server, for when k3helper reaches the nodes over one network and the
  cluster talks over another.
- **`ssh.Client.WriteFileFrom`** streams a file to a node with progress,
  instead of holding it in memory. The airgap image archive is 184MB.

### Fixed

- **Agents joined on the wrong address.** The join address was discovered with
  `hostname -I`, which returns a cloud VM's public address first — the one
  address an air-gapped network cannot reach. Agents retried "failed to get CA
  certs" indefinitely while the server ran fine beside them. It now comes from
  the targets file: the address the operator chose, and the one k3helper has
  just proved works by connecting over it.
- **`%!w(<nil>)` in install failures.** A command that ran and exited non-zero
  has no error to wrap, and the `%w` verb printed its own failure as the last
  thing an operator saw when an install failed.
- **A healthy managed cluster scored 0% in the TUI.** Every host check is
  skipped on a kubeconfig cluster, and skips were counted as "not OK". A skip
  is now left out of both halves of the fraction, so the score reads `n/a`
  rather than putting the most alarming number on screen for a healthy cluster.

### Testing

- `test/do/do.sh` provisions, air-gaps and destroys DigitalOcean droplets
  through the v2 API for live scenario testing. Everything it creates is tagged
  `k3helper-test`, so teardown can never touch anything else.

## v0.5.0

Clusters you cannot SSH into. k3helper now reaches a cluster either by its
nodes or by a kubeconfig, which is what a managed cluster (EKS, GKE, AKS,
Rancher, anything else that hands you credentials and keeps the machines)
actually gives you.

### Install

```bash
curl -fsSL https://raw.githubusercontent.com/solutionforest/k3helper/main/install.sh | sh
```

No breaking changes from v0.4.0. Existing targets files keep working exactly
as they did.

### Added

- **`kubeconfig:` clusters.** A targets file entry with `kubeconfig:` (and an
  optional `kube_context:`) instead of `nodes:` drives a local `kubectl`
  against the API server. `k3helper init --kubeconfig ~/.kube/config
  --kube-context prod` writes one.
- **Everything that works through the API server works there**: `doctor`'s
  cluster and workload signatures, `deploy`, `verify --dry-run-server`, `gen`,
  and the TUI including logs, describe and port-forward.
- **`k3helper ctx` gained a REACHED column**, so `0 nodes` reads as "reached by
  kubeconfig" rather than as a broken file. One file can mix both kinds.
- **Windows binaries, released.** `k3helper-windows-amd64.exe` and
  `k3helper-windows-arm64.exe` are built by `make release`, listed in
  `checksums.txt`, and published with every tag. One file, no installer,
  nothing written outside the folder you put it in. The kubeconfig transport
  runs kubectl by argument rather than through a shell, so it needs no
  `/bin/sh`.
- **`make portable-check`**, and CI runs it on Linux, macOS and Windows.
  "Single portable binary" is a claim about the runtime rather than the build,
  so it is checked by running the binary from a directory it has never seen:
  it must start, explain a missing targets file, refuse a kubeconfig that is
  not there without writing anything, complete the kubeconfig and SSH init
  flows, generate and verify YAML with no cluster and no network, and leave
  nothing behind in `$HOME`.
- **A macOS CI job.** macOS is what most operators drive a cluster from and
  nothing in CI ran there before; it now gets vet, race tests and the portable
  check on every push, the same as Linux.

### Fixed

- **`doctor` now catches a crash loop it happens to sample mid-restart.** A
  container in backoff only reads as CrashLoopBackOff while it is waiting
  between attempts; the moment it starts again the pod is Running with no
  reason attached, and the diagnosis saw nothing worse than an unready pod.
  Restart counts were already parsed and thrown away — they are now kept, and a
  pod that is not ready after three or more restarts is called a crash loop
  whichever half of the cycle we caught. This is why the fault matrix could
  catch that fault or miss it depending on timing.

### Changed

- **`doctor` separates scope notes from faults.** A finding that describes what
  could *not* be looked at — incomplete evidence, or a missing host layer — is
  printed under "Notes on what was looked at" and no longer sets the exit code.
  A healthy managed cluster exits 0 instead of 2.
- **`check` and the TUI build their host check list from one place**
  (`check.HostChecks`), so a check added to one cannot go missing from the
  other.

### Not available on a kubeconfig cluster

By nature, not by omission — a kubeconfig reaches the API server, not the
machines behind it:

- Host-layer checks: disk, memory, swap, cgroups, systemd units, container
  runtime, clock skew. `doctor` reports the gap rather than implying those are
  fine; `check` says it has nothing to check.
- `vm setup` and `registry apply`. Both write files on the nodes, and both
  refuse with an explanation rather than doing half the job.
- Certificate expiry, which reads `k3s certificate check` on the node.

## v0.4.0

Private registries: declared once, applied to every node, and diagnosed
properly when a pull fails.

### Install

```bash
curl -fsSL https://raw.githubusercontent.com/solutionforest/k3helper/main/install.sh | sh
```

Or take a single binary — `k3helper-{linux,darwin}-{amd64,arm64}`, each listed
in `checksums.txt`. No breaking changes from v0.3.0.

### Registries in the targets file

```yaml
registries:
  - host: docker-registry.example.net
    username: ci
    password_env: REGISTRY_PASSWORD          # read from the environment, not stored here
    ca_file: /etc/ssl/certs/internal-ca.crt  # path ON THE NODES
```

Cluster-level, not per-node: a mirror configured on two machines out of three
produces pods that run in some places and not others.

- `vm setup` writes them **before** the install — the first thing a fresh node
  does is pull images, so a mirror applied afterwards is too late for exactly
  the pulls an air-gapped or credentialled environment needs it for.
- `k3helper registry apply` pushes them to a cluster that already exists and
  restarts k3s, which does not re-read the file on its own. `--no-restart` to
  pick your own moment; `registry show` prints what would be written with the
  password redacted.
- `password_env` keeps the credential out of a file that goes into a
  repository. An unset variable fails at apply time rather than becoming an
  anonymous pull that fails later, on another machine, as "unauthorized".
- The password never reaches a command line: the file is staged 0600 and
  installed with sudo, because a `sudo tee` pipeline puts the credential in
  `ps` output for every user on the machine.
- One-off without editing the file: `--registry`, `--registry-user`,
  `--registry-password`, `--registry-ca-file`, `--registry-insecure`.

### Pull secrets and imagePullSecrets

```bash
k3helper gen secret sf-registry --docker-registry docker-registry.example.net \
  --registry-user ci --registry-password "$PW"
k3helper gen deployment web -i docker-registry.example.net/app:1.2 --image-pull-secret sf-registry
```

`--image-pull-secret` reaches the pod spec of every kind that has one.

### ImagePullBackOff now says *why*

Three new signatures — `registry.auth`, `registry.cert`,
`registry.unreachable` — read the reason out of the kubelet's pull-failure
events and rank above the generic `pod.imagepull`, which is demoted when a
cause is known. One symptom covered four different problems and four different
fixes; sending someone to check credentials when the registry name never
resolved was the failure worth removing.

`check` gained `registry.config`, which catches what only shows up at pull
time: a `registries.yaml` that does not parse (k3s ignores the whole file, so
every private pull silently becomes an anonymous public one), a `ca_file` that
was never copied to the node, TLS verification left off.

### kubeadm

Mirrors, CAs and `insecure_skip_verify` are written to containerd's
`/etc/containerd/certs.d`, and containerd is pointed at that directory —
without which the files are written and ignored. Credentials on this path are
refused rather than written: containerd's auth schema moved between 1.x and
2.x, and writing one we cannot verify on the node fails silently at pull time.
The error names the alternative.

### Fixed

A `registries:` block in a single-cluster targets file was parsed and then
dropped on load, so it configured nothing. Registries can also be declared at
file level in a multi-cluster file, where they apply to every cluster that
does not declare its own.

## v0.3.0

The TUI grew from four views to fifteen, CI now runs the whole product on
GitHub-hosted runners, and a defect that made k3helper misreport a running
cluster was found by doing so.

### Install

```bash
curl -fsSL https://raw.githubusercontent.com/solutionforest/k3helper/main/install.sh | sh
```

Or take a single binary — `k3helper-{linux,darwin}-{amd64,arm64}`, each listed
in `checksums.txt`. No breaking changes from v0.2.0.

### Fixed: a running k3s reported as "not installed"

`check` and `doctor` decided whether k3s was installed by asking `systemctl
list-unit-files` without sudo. On a host whose SSH login cannot reach the
systemd bus — no dbus, or a locked-down login — that query answers "Failed to
connect to bus" for every unit, so:

- `check` reported **"no Kubernetes service found"** on nodes where k3s was up
  and serving traffic, and
- `doctor` could not see a **stopped `k3s-agent`**, which is the single fault
  the troubleshooter most needs to catch.

Unit presence is now read from the filesystem, where it is a fact rather than a
question for a bus that may not answer. Unit *state* is asked unprivileged
first and with `sudo -n` second, because neither works everywhere: a host
without passwordless sudo answers only the first, a host without bus access
only the second. When neither answers, the state is reported as unknown — an
unreadable probe is never rendered as a stopped service, nor as a healthy one.

If you run k3helper against hosts that do not ship dbus, or where the login
user has no systemd access, upgrade.

### TUI: the rest of the resource browser

New views, all reachable from the command bar: `:dp` deployments, `:sts`
statefulsets, `:ds` daemonsets, `:svc` services, `:ing` ingresses, `:doctor`
(findings ranked by confidence, `enter` for the evidence and the fix), `:xray`
(deployment → replicaset → pod ownership graph), `:vm` (the targets file probed
live, `b` bootstraps every node with streaming output), `:ctx` (switch
cluster), `:gen` (YAML studio: generate, verify inline, save) and `:deploy
<file>` (dry-run, diff against live state, `a` applies).

`enter` on a deployment or service drills into *its* pods using the workload's
own label selector, rather than guessing from the name.

### TUI: navigation and presentation

- `/` filters with a regular expression; a filter starting with `-l` is a
  kubectl label selector, evaluated by the API server
- `ctrl-w` wide mode, `ctrl-z` faults only, `N`/`A`/`S` sort by name, age or
  status — ages sort as durations, so "3d" no longer sorts before "12m"
- three skins (`--theme dark|light|k3s-orange`, or a skin YAML file), and
  `:theme` to switch without restarting
- an always-visible status bar with the cluster health score and doctor's
  finding count
- CPU and memory sparklines on each node card, sampled from `/proc` over the
  SSH connection the dashboard already holds — no metrics-server needed, and
  they still work for a node the API server has lost sight of
- progress bars for deploy rollout and node bootstrap; syntax-highlighted
  YAML, describe and diff panes

### CI runs the whole product now

The container sandbox driver works, so `ubuntu-latest` runs the full E2E, the
integration tests and the fault matrix on every push, instead of a
dispatch-only job nobody had seen pass. Three environment fixes: containerd
state on named volumes (overlayfs cannot stack on itself), a private cgroup
namespace (the container's systemd was pruning containerd's pod cgroups), and
flannel `host-gw` instead of VXLAN (the containers share one bridge).

The fault matrix now includes `disk-full`, which brings it to 11 faults. It
runs last: kubelet holds DiskPressure for five minutes after the disk is free.

### Scope

Machine provisioning — Multipass, Vagrant, cloud provider CLIs — has been
dropped from the plan rather than left as a promise. Each wraps a tool you
already have, none touches the hard part, and every one costs a runtime
dependency. A machine that answers SSH is the input.

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
