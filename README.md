# k3helper ☸

**A portable k3s/k8s helper in a single binary** — set up VMs, verify & generate YAML, quick-deploy, and find issues fast, with a TUI.

```
k3helper is a portable k3s/kubernetes helper with a TUI.

  init        create a targets.yaml describing your nodes
  vm setup    install k3s on target VMs over SSH
  verify      validate Kubernetes YAML manifests
  gen         generate correct Kubernetes YAML
  deploy      quick deploy manifests to the cluster
  check       run cluster/node/k3s health checks
  doctor      troubleshoot: find issues + remediation
  ctx         list clusters defined in the targets file
  tui         launch the interactive dashboard
```

## Why

Existing tools each cover one slice:

| Tool | Does | Doesn't |
|---|---|---|
| k9s | browse/manage a running cluster | VM setup, YAML gen/verify, host-layer diagnostics, guided fixes |
| k3sup | VM → k3s over SSH in 60s | troubleshooting, YAML, checks |
| Popeye | live cluster linting | can't see the VM host or the k3s service; emits codes, not fixes |
| kubeconform | offline YAML validation | no generation, no cluster/host access |

**k3helper spans all layers** — VM host → k3s service → cluster → workloads → YAML — and its `doctor` ranks likely root causes *with the fix attached*. That cross-layer diagnosis is validated by a fault-injection test suite.

## Install

### One-liner

```bash
curl -fsSL https://raw.githubusercontent.com/solutionforest/k3helper/main/install.sh | sh
```

Detects your OS/arch, downloads the latest release into `/usr/local/bin`, and verifies it against the release `checksums.txt`. It only reaches for `sudo` if the install directory isn't writable.

Override version or location:

```bash
curl -fsSL https://raw.githubusercontent.com/solutionforest/k3helper/main/install.sh \
  | K3HELPER_VERSION=v0.1.0 K3HELPER_BIN_DIR="$HOME/.local/bin" sh
```

### Single file into the current folder

No installer, no sudo, nothing written outside your working directory:

```bash
uname -sm                                    # e.g. "Linux x86_64" → linux-amd64
curl -fsSL -o k3helper \
  https://github.com/solutionforest/k3helper/releases/latest/download/k3helper-linux-amd64
chmod +x k3helper
./k3helper version
```

Assets: `k3helper-linux-amd64`, `k3helper-linux-arm64`, `k3helper-darwin-amd64`, `k3helper-darwin-arm64`. Every release also ships `checksums.txt`.

### From source

```bash
git clone https://github.com/solutionforest/k3helper && cd k3helper
make build          # ./bin/k3helper (current platform)
make release        # stripped binaries + checksums.txt in dist/
```

Requirements: Go 1.22+ to build. At runtime, nothing — it's a static binary. On the nodes: `curl` (for the k3s install script) and passwordless `sudo`.

## Quick start

### 0. Confirm your machine can reach the nodes

k3helper drives your nodes over SSH **from wherever you run it**, using key auth only — it never prompts for a password, and it runs privileged commands with `sudo -n`. Both must already work. Check every node before going further:

```bash
for h in 10.0.0.10 10.0.0.11; do
  ssh -i ~/.ssh/id_ed25519 -o BatchMode=yes ubuntu@$h 'echo ssh ok; sudo -n true && echo sudo ok'
done
```

You want `ssh ok` **and** `sudo ok` from each. Common fixes:

| Symptom | Cause | Fix |
|---|---|---|
| `Permission denied (publickey)` | your key isn't on the node | `ssh-copy-id -i ~/.ssh/id_ed25519.pub ubuntu@10.0.0.10` |
| password prompt appears | key auth not in use | that's what `BatchMode=yes` proves; fix the key first |
| `sudo: a password is required` | no passwordless sudo | on the node: `echo 'ubuntu ALL=(ALL) NOPASSWD:ALL' \| sudo tee /etc/sudoers.d/ubuntu` |
| `i/o timeout` | firewall / wrong address | open port 22 from your machine, or see [browser-console-only setup](#no-ssh-from-your-machine-browser-console-only) |

**No inbound SSH at all — only a browser console?** Skip to [that section](#no-ssh-from-your-machine-browser-console-only); the flow is different.

### 1. Describe your nodes

```bash
k3helper init --server 10.0.0.10 --agent 10.0.0.11 \
    --user ubuntu --key ~/.ssh/id_ed25519
# ✓ wrote targets.yaml
```

`k3helper init --local` describes this machine instead, and plain `k3helper init`
writes a template to edit. The file it produces:

```yaml
cluster: my-cluster
nodes:
  - name: server
    role: server          # server | agent
    host: 10.0.0.10
    port: 22
    user: ubuntu
    key: ~/.ssh/id_ed25519
  - name: worker1
    role: agent
    host: 10.0.0.11
    port: 22
    user: ubuntu
    key: ~/.ssh/id_ed25519
```

`port` defaults to `22`. A node can instead set `local: true` and omit `host`/`user`/`key` — that's the machine k3helper is running on, and it's how the [browser-console flow](#no-ssh-from-your-machine-browser-console-only) works.

#### SSH host keys

Host keys are verified against `~/.ssh/known_hosts` by default. A first
connection to an unrecorded host is refused, with the fingerprint and the ways
forward:

```
host 10.0.0.10:22 is not in known_hosts (key SHA256:cxF1AFz7...).
Verify the fingerprint out of band, then either:
  ssh-keyscan -H 10.0.0.10 >> ~/.ssh/known_hosts
or re-run with --accept-new-host-key to trust it on first use,
or --insecure-host-key to skip verification entirely.
```

`--accept-new-host-key` trusts unknown hosts on first use and records them.
A key that *changed* is always refused, in every mode — that is the case worth
stopping for. Per node, `insecure_host_key: true` in the targets file skips
verification for hosts whose address churns (the test sandbox does this).

### 2. Install k3s on all nodes

```bash
k3helper vm setup -t targets.yaml \
  --server-extra-args "--disable=traefik" \
  --kubeconfig ./kubeconfig      # fetched back to your machine
# ✓ cluster ready
```

The fetched kubeconfig has its server address rewritten from k3s's `127.0.0.1`
to the node's real address, so it works from your machine as-is.

`vm setup` waits for every node in the targets file to register *and* go Ready,
not just for whichever have registered so far — otherwise "cluster ready" can
mean a server with no agents attached.

### 3. Check health

```bash
k3helper check -t targets.yaml
```
```
== node server (server) ==
✓  OK      disk 46% used
✓  OK      6129MB memory available
✓  OK      cgroup controllers present
✓  OK      k3s is active
== node worker1 (agent) ==
...
```
Exit code: `0` when nothing failed, `2` when a check failed or a node was
unreachable. **Warnings do not change the exit code** — a swap warning should
not fail a pipeline the same way a dead k3s does. `--strict` escalates them.

The service check adapts to the node: `k3s`/`k3s-agent` on a k3s host,
`kubelet` + `containerd` on a kubeadm one, detected from the unit files that
are actually installed. A node with neither is told to install one, not to
restart something that was never there.

### 4. Generate & verify YAML

```bash
k3helper gen deployment web -i nginx:1.25 -r 3 -p 8080 -o web.yaml
k3helper verify web.yaml            # → ✓ Deployment/web (apps/v1)
```

Supported kinds: Deployment, StatefulSet, DaemonSet, Pod, Service, Ingress,
ConfigMap, Secret, PersistentVolumeClaim, Namespace, Job, CronJob — by full
name, any casing, or the kubectl short alias (`pvc`, `deploy`, `svc`, `sts`,
`ds`, `ns`, `cm`, `ing`, `cj`, `po`).

`verify` runs three layers:

| Layer | Needs a cluster? | Catches |
|---|---|---|
| 1. Syntax | no | malformed YAML |
| 2. Structure | no | missing `apiVersion` / `kind` / `metadata.name` |
| 3. Schema (offline) | no | per-kind shape errors — a Job with `restartPolicy: Always`, a DaemonSet with `spec.replicas`, a container with no image |
| 3b. Schema (live) | yes | everything else: unknown fields, admission webhooks, CRD schemas |

```bash
k3helper verify web.yaml --dry-run-server -t targets.yaml
```

Layer 3b submits the manifest to the real API server via `kubectl apply --dry-run=server`. Unknown kinds (CRDs) get layers 1–2 only — k3helper doesn't guess at schemas it doesn't know.

Every kind `gen` emits is checked against a **live API server** in the integration suite, not just against `verify`.

### 5. Deploy

```bash
k3helper deploy -f web.yaml -t targets.yaml
# ✓ applied deployment.apps/web
# ✓ rolled out deployment.apps/web
```

`-f` is a path on **your** machine: the manifest is uploaded to a temporary file on the server, applied, then removed. Server-side dry-run validation runs first; `--dry-run` stops there.

`-n/--namespace` overrides the target namespace. If a document declares its own `metadata.namespace` that disagrees with the flag, the deploy is refused rather than silently redirected:

```
Error: --namespace staging conflicts with the manifest: Service/web declares
namespace "prod"; remove metadata.namespace or drop the flag
```

The namespace must already exist — `deploy` never creates cluster state the manifest didn't ask for.

`--diff` shows what would change against live cluster state before anything is applied:

```bash
k3helper deploy -f web.yaml -t targets.yaml --diff --dry-run
```
```diff
--- live
+++ merged
-  replicas: 1
+  replicas: 3
       containers:
-      - image: nginx:1.24
+      - image: nginx:1.25
```

Pair it with `--dry-run` to look without touching anything. When the manifest already matches the cluster you get `= no changes against live cluster state`.

### Multiple clusters

One targets file can describe several clusters:

```yaml
clusters:
  - cluster: prod
    nodes: [...]
  - cluster: staging
    nodes: [...]
current: prod        # optional; defaults to the first
```

```bash
k3helper ctx -t targets.yaml            # list them
k3helper doctor -t targets.yaml --context staging
```
```
   CLUSTER  NODES  SERVER
*  prod     3      10.0.0.10
   staging  1      10.0.1.10
```

`--context` works on every subcommand. The single-cluster format (`cluster:` and `nodes:` at the top level) keeps working unchanged — a one-cluster file never has to grow a list.

### 6. Find issues fast

```bash
k3helper doctor -t targets.yaml
```
Healthy:
```
✓ no issues detected — cluster looks healthy
```
Broken (k3s stopped on a node):
```
Found 1 likely issue(s), ranked by confidence:

1.  [90%]  Node NotReady because k3s service is down
    fix:   On the affected node: `sudo systemctl restart k3s` (or k3s-agent), then check
          `sudo journalctl -u k3s -n 100 --no-pager` for the underlying cause.
```
Doctor gathers evidence across every layer — VM host (`df`, `free`, cgroups, systemd), the k3s service, node conditions, pod statuses, container states, events, endpoints, TLS certificates — then matches it against a signature knowledge base. Exit code `2` when findings exist.

| Layer | Signatures |
|---|---|
| Workload | ImagePullBackOff · CrashLoopBackOff · OOMKilled · unschedulable (resources/taints/cordon) · running but never ready · evicted |
| Node | service down · NotReady with a running service · container runtime down · DiskPressure · MemoryPressure · clock skew · image-cache bloat · unreachable over SSH |
| Cluster | embedded-etcd quorum lost or at risk · TLS certificates expiring · kubeconfig/auth broken · evidence that could not be gathered |
| Network / storage | CoreDNS has no ready replicas · Services with no ready endpoints · PVC stuck Pending |

Ranking is by confidence, so a cause floats above the symptoms it produces: a
stopped k3s outranks "kubeconfig invalid", a CoreDNS outage outranks the
Services it takes down, and a crashlooping pod outranks its Service having no
endpoints.

Signatures stay silent rather than guessing when the evidence they need is
absent — a sqlite-backed single-server cluster has no etcd, so the quorum
signature never fires there. And what could **not** be gathered is reported
rather than quietly narrowing the diagnosis:

```
! could not gather pvcs: kubectl get pvc failed (exit 1)
```

`doctor --watch 30s` re-runs on an interval, redrawing only when the verdict
changes so a long watch does not bury the moment things went wrong. It
reconnects each pass — a node going away is one of the things being watched
for, and a held-open connection would keep reporting the last state it saw.

`doctor --json` emits the same information as `probe_errors`, for scripts. An
incomplete gather alone does not fail the exit code — a kubeconfig scoped away
from one resource should not turn every run red — but it is always stated, so
"no issues detected" never silently means "did not look".

A node it cannot reach is reported, never skipped — partial inspection must not read as a clean bill of health:

```
✗ node agent2 unreachable: ssh dial sandbox@10.0.0.12:22: dial tcp: i/o timeout

1.  [85%]  Node unreachable over SSH (no evidence could be gathered)
```

### 7. TUI

```bash
k3helper tui -t targets.yaml
```

Opens on a dashboard of per-node check cards with severity colours and remediation hints, and carries a k9s-style resource browser.

**Navigation** — press `:` for the command bar:

| Command | Shows |
|---|---|
| `:pods` (`:po`) | pods across all namespaces: ready, status, restarts, node, age |
| `:nodes` (`:no`) | nodes: status, roles, kubelet version, age |
| `:events` (`:ev`) | recent events, newest first |
| `:dashboard` (`:dash`) | back to the health cards |
| `:ns <name>` | scope resource views to one namespace (`:ns all` clears it) |

**Keys** — `↑↓` move · `enter`/`l` pod logs · `d` describe · `/` filter · `esc` back or clear filter · `r` refresh now · `q` quit.

The status column shows the container's waiting or terminated reason rather than the pod phase, so a `CrashLoopBackOff` reads as `CrashLoopBackOff` instead of `Pending`. Opening logs on a crashlooping pod whose current instance has produced nothing falls back to the previous instance automatically — that's where the cause usually is.

Views reload every 5 seconds, preserving your cursor position so a refresh doesn't move the row under you.

## No SSH from your machine (browser console only)

Some providers hand you nothing but a web terminal on the VM — no inbound SSH, no `scp`. k3helper covers this by running **on the server node itself**. Mark that node `local: true` in `targets.yaml` and its commands are executed directly through `/bin/sh` instead of over SSH: no sshd, no loopback key, no self-connection. If you're already root, it also skips `sudo`, which minimal console-only images often don't ship.

### 1. Get the binary onto the server

Open the web console, then — if the server can reach the internet (it needs to anyway, `vm setup` pipes `get.k3s.io`):

```bash
curl -fsSL https://raw.githubusercontent.com/solutionforest/k3helper/main/install.sh | sh
k3helper version
```

**No outbound internet?** Build a paste bundle on your machine and paste it in through the console:

```bash
make bundle                      # dist/bundle/ — linux/amd64, gzip
PLATFORM=linux/arm64 make bundle
CHUNK_LINES=500 make bundle      # smaller pastes for consoles that reject big ones
```

That produces numbered snippets. Paste `001-paste.sh`, `002-paste.sh`, … into the console in order — each prints a running line count — then paste `999-install.sh`. The installer refuses to proceed unless the line count, byte size and SHA-256 all match, so a dropped or truncated paste fails loudly instead of installing a corrupt binary. Read `dist/bundle/000-README.txt` first.

### 2. Write targets.yaml on the server

For a **single-node cluster**, one command is the whole step:

```bash
k3helper init --local
```

A server **plus** agents is a mixed inventory — one local node and some over SSH — which `--local` deliberately refuses to generate (`--local describes a single node; do not combine it with --server/--agent`). Write it by hand:

```yaml
cluster: prod
nodes:
  - name: server
    role: server
    local: true              # this machine — no host/user/key needed
  - name: agent1
    role: agent
    host: 10.0.0.11          # reachable from the server, on the private network
    user: root
    key: /root/.ssh/k3helper
  - name: agent2
    role: agent
    host: 10.0.0.12
    user: root
    key: /root/.ssh/k3helper
```

Exactly one node may be `local: true`. For a **single-node cluster**, that one entry is the whole file — you're done with this step.

### 3. Let the server SSH to the agents

The server still reaches agents over SSH; only your laptop is cut out. On the server:

```bash
ssh-keygen -t ed25519 -f /root/.ssh/k3helper -N ''
cat /root/.ssh/k3helper.pub
```

Copy that one line (~100 chars — pastes fine even in a sluggish VNC console) and, in **each agent's** console:

```bash
mkdir -p ~/.ssh && chmod 700 ~/.ssh
echo 'ssh-ed25519 AAAA... k3helper' >> ~/.ssh/authorized_keys
chmod 600 ~/.ssh/authorized_keys
```

Verify from the server before continuing — same preflight as [step 0](#0-confirm-your-machine-can-reach-the-nodes), just run from a different machine:

```bash
ssh -i /root/.ssh/k3helper -o BatchMode=yes root@10.0.0.11 'echo ssh ok; sudo -n true && echo sudo ok'
```

That `ssh` writes the agents into the server's `known_hosts` as a side effect, which is also what k3helper's own [host-key verification](#ssh-host-keys) needs — do it for every agent, or pass `--accept-new-host-key` on the first k3helper command below.

### 4. Bootstrap

```bash
k3helper vm setup -t targets.yaml --kubeconfig /root/.kube/config
k3helper check  -t targets.yaml
k3helper doctor -t targets.yaml
```

The `local: true` server is never dialled, so it needs no host key at all — only the agents do.

Everything else — `check`, `doctor`, `deploy`, `verify --dry-run-server`, `tui` — works the same from here, with the local node checked in-process and the agents over SSH.

### Ports between nodes

`vm setup` installs k3s but does not touch your firewall. Agents must reach the server on:

| Port | Proto | For |
|---|---|---|
| 6443 | TCP | Kubernetes API (agent → server) |
| 8472 | UDP | flannel VXLAN (all nodes, both ways) |
| 10250 | TCP | kubelet metrics (all nodes, both ways) |

Missing 8472/UDP is the classic one: nodes go `Ready`, then pod-to-pod traffic across nodes silently dies.

### If the agents can't be reached from the server either

Then nothing can drive them remotely, and k3s's own join command is the fallback. On the server:

```bash
k3helper vm setup -t targets.yaml          # server only: just the local: true node
cat /var/lib/rancher/k3s/server/node-token
hostname -I | awk '{print $1}'             # the address agents will dial
```

In each agent's console:

```bash
curl -sfL https://get.k3s.io | K3S_URL=https://<server-ip>:6443 K3S_TOKEN=<token> sh -
```

Back on the server, `k3s kubectl get nodes` should show them joining. You lose `check`/`doctor` coverage for those agents — k3helper reports an unreachable node rather than skipping it, so it will say so — but the cluster itself is complete.

> The join token authenticates any machine to your cluster. Treat it like a password: don't paste it into a shared terminal recording, a ticket, or a chat log.

## k3s vs k8s support

| Area | k3s | kubeadm / other k8s |
|---|---|---|
| verify / gen | ✅ | ✅ (pure YAML) |
| deploy / doctor cluster layer | ✅ | ✅ (kubectl-based) |
| check host service layer | ✅ (`k3s`/`k3s-agent`) | ✅ (`kubelet` + `containerd`) |
| kubeconfig discovery | ✅ `/etc/rancher/k3s/k3s.yaml` | ✅ `/etc/kubernetes/admin.conf` |
| certificate expiry | ✅ (`k3s certificate check`) | ❌ k3s-specific command |
| vm setup | ✅ | ❌ install script is k3s-specific |

The distribution is detected per node from its unit files, so a mixed
inventory works: kubectl is invoked through whichever kubeconfig exists, and
the service check looks for the units that distribution actually installs.

## Testing

The repo ships a self-contained sandbox: three Ubuntu 24.04 hosts that k3helper installs k3s onto from scratch.

```bash
make sandbox-up      # 3 hosts + SSH
make bootstrap       # k3helper installs k3s on all nodes
make check / doctor  # live cluster
make e2e             # full lifecycle, ~10-15 min
make e2e-fast        # reuse running sandbox, ~8 min
make e2e-quick       # reuse sandbox, skip the fault sweep, ~4 min
make test            # unit tests
make test-integration
make fault-list      # every fault + the signature it should trigger
make fault-<name>    # inject one fault, e.g. make fault-oom
make fault-check-all # inject each fault, assert doctor diagnoses it
make fault-clean     # undo them
make release         # stripped binaries + checksums.txt in dist/
make release-upload  # cut/refresh the GitHub release and upload dist/
make bundle          # offline paste bundle (see the browser-console section)
```

**The troubleshooter's exam.** `make fault-check-all` injects each fault in turn, asserts `doctor` reports the signature that fault is supposed to produce, then cleans up:

| Fault | Expected signature |
|---|---|
| `imagepull` · `crashloop` · `oom` | `pod.imagepull` · `pod.crashloop` · `pod.oom` |
| `pending` · `cordon` | `pod.pending-sched` |
| `pvc-pending` | `storage.pvc-pending` |
| `coredns` · `empty-endpoints` | `network.coredns` · `network.empty-endpoints` |
| `k3s-down` · `disk-full` | `node.notready-k3s-down` · `node.diskpressure` |
| `bad-kubeconfig` | `cluster.kubeconfig` |

Matching is on signature IDs from `doctor --json`, not display titles, so rewording a finding cannot silently break the suite.

`make e2e` is the full product exercised end to end against three fresh VMs —
`init`, host-key verification (including a *changed* key), bootstrap, the
fetched kubeconfig actually connecting from your machine, every generated kind
against the live API server, deploy/diff/namespaces, contexts, `doctor` on a
healthy and a faulted cluster, and the fault sweep. `make e2e-quick` skips the
sweep for a fast loop.

The sandbox has two drivers, selected automatically and overridable with
`SANDBOX_DRIVER`:

| Driver | Hosts | Where |
|---|---|---|
| `orbstack` | three Linux VMs | macOS default; what the E2E results above were produced on |
| `docker` | three privileged systemd containers | Linux, and anywhere VMs are unavailable |

The container driver provisions hosts correctly and k3s installs and reaches
Ready on them, but it is **not yet proven end to end**: on the author's machine
(Docker inside an OrbStack Linux VM) CoreDNS is repeatedly SIGTERMed because
kubelet's probes cannot reach pod IPs through flannel. Whether a native Linux
runner behaves the same is untested. The CI E2E job therefore runs on
`workflow_dispatch` only — run it by hand to find out, rather than blocking
every push on an unverified job. Promote it to `push` once it passes.

CI on every push: gofmt, `go vet` (including under the `integration` build tag,
so those files cannot rot unnoticed), race-enabled unit tests, and a
cross-compile.

Integration tests (`-tags=integration`) read node addresses from `test/sandbox/targets.sandbox.yaml` rather than hardcoding them, because the sandbox is assigned new IPs each time it is recreated. Point them at another cluster with `K3HELPER_TARGETS=/path/to/targets.yaml`. With no sandbox running they skip rather than fail.

> **On macOS, use the `orbstack` driver.** [OrbStack](https://orbstack.dev) gives real lightweight Linux VMs. Docker containers on macOS share the host kernel, which breaks kubelet's PLEG and kills pods falsely — so the `docker` driver is for Linux, not for a Mac.

## Architecture

```
cmd/k3helper/            entry point
install.sh               one-liner installer (POSIX sh, checksum-verified)
scripts/bundle.sh        offline paste bundle for air-gapped web consoles
internal/
  config/    targets.yaml loading + validation, single- and multi-cluster
  ssh/       node transport: SSH client (key auth, known_hosts verification,
             run/stream/sudo) or, for `local: true` nodes, direct /bin/sh
  vm/        k3s bootstrap over SSH (server → token → agents → wait Ready)
  kyaml/     YAML verify (3 layers, offline + live) + generate (12 kinds)
  kube/      cluster reads through kubectl: pods, nodes, events, logs, describe
  sandbox/   locates the test sandbox from targets.sandbox.yaml
  deploy/    dry-run → diff → apply → rollout wait
  check/     check registry {status, summary, evidence, remediation} +
             per-node k3s/kubeadm distro detection
  troubleshoot/  evidence gathering + signature matching + diagnosis ranking
  tui/       Bubble Tea dashboard + k9s-style resource browser
  cli/       cobra commands
test/
  sandbox/   3-host sandbox, orbstack (VMs) and docker (containers) drivers
  faults/    fault injection + the troubleshooter's exam (check-all.sh)
  fixtures/  YAML corpus, goldens
  e2e.sh     full-lifecycle E2E
```

## Known limitations

Current, and worth knowing before pointing this at production:

- **Host-layer checks assume systemd.** Nodes without it (some minimal or
  container-based images) get cluster-layer findings only.
- **`doctor` reads the cluster through the first server node.** If that node is
  down, cluster-layer evidence is unavailable even when other servers are up.
- **Certificate expiry is read via `k3s certificate check`**, so it is not
  collected on kubeadm clusters. `doctor` says so rather than implying the
  certificates are fine.
- **`vm setup` only installs k3s.** A kubeadm cluster can be checked and
  diagnosed, but not bootstrapped.
- **Clock skew is measured against the machine running k3helper**, so a laptop
  with a wrong clock will accuse every node.
- **A cluster member missing from the targets file is invisible.** Host
  evidence comes from the file, not from `kubectl get nodes`, so a node nobody
  listed contributes nothing and is not reported as unreachable.

## Roadmap

- [ ] HA control plane: multiple servers, `--cluster-init`, embedded etcd setup
- [ ] Port-forward manager and multi-pod log tailing in the TUI
- [ ] `doctor --watch` for continuous monitoring
- [ ] Get the container sandbox passing the E2E so CI can run it on every push
- [ ] `vm setup` for kubeadm, so bootstrap covers both distributions it can check

## License

MIT
