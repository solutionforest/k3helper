# k3helper ☸

**A portable k3s/k8s helper in a single binary** — set up VMs, verify & generate YAML, quick-deploy, and find issues fast, with a TUI.

```
k3helper is a portable k3s/kubernetes helper with a TUI.

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

`targets.yaml`:

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

### 2. Install k3s on all nodes

```bash
k3helper vm setup -t targets.yaml \
  --server-extra-args "--disable=traefik" \
  --kubeconfig ./kubeconfig      # fetched back to your machine
# ✓ cluster ready
```

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
Exit code: `0` healthy · `2` findings.

### 4. Generate & verify YAML

```bash
k3helper gen deployment web -i nginx:1.25 -r 3 -p 8080 -o web.yaml
k3helper verify web.yaml            # → ✓ Deployment/web (apps/v1)
```

Supported kinds: Deployment, StatefulSet, DaemonSet, Pod, Service, Ingress, ConfigMap, Secret, PVC, Namespace, Job, CronJob.

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
@@ -29,7 +29,7 @@
       containers:
-      - image: nginx:1.24-alpine
+      - image: nginx:1.25-alpine
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
| Workload | ImagePullBackOff · CrashLoopBackOff · OOMKilled · unschedulable (resources/taints) · evicted |
| Node | k3s service down + NotReady · DiskPressure · MemoryPressure · unreachable over SSH |
| Cluster | embedded-etcd quorum lost or at risk · TLS certificates expiring · kubeconfig/auth broken |
| Network / storage | CoreDNS has no ready replicas · Services with no ready endpoints · PVC stuck Pending |

Ranking is by confidence, so a cause that explains the others floats to the top — a total CoreDNS outage outranks the individual Services it takes down. Signatures that need evidence k3s doesn't have stay silent rather than guessing: a sqlite-backed single-server cluster has no etcd, so the quorum signature never fires there.

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

### 4. Bootstrap

```bash
k3helper vm setup -t targets.yaml --kubeconfig /root/.kube/config
k3helper check  -t targets.yaml
k3helper doctor -t targets.yaml
```

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

| Area | k3s | other k8s |
|---|---|---|
| verify / gen | ✅ | ✅ (pure YAML) |
| deploy / doctor cluster layer | ✅ | ✅ (kubectl-based) |
| check host service layer | ✅ (`k3s`/`k3s-agent` units) | ❌ unit names differ (`kubelet`, `containerd`) |
| vm setup | ✅ | ❌ install script is k3s-specific |

## Testing

The repo ships a self-contained sandbox: 3 Ubuntu 24.04 VMs (OrbStack) that k3helper installs k3s onto from scratch.

```bash
make sandbox-up      # 3 VMs + SSH
make bootstrap       # k3helper installs k3s on all nodes
make check / doctor  # live cluster
make e2e             # full lifecycle, ~10-15 min
make e2e-fast        # reuse running sandbox, ~6 min
make test            # unit tests
make test-integration
make release         # stripped binaries + checksums.txt in dist/
make release-upload  # cut/refresh the GitHub release and upload dist/
make bundle          # offline paste bundle (see the browser-console section)
```

The E2E script proves the whole loop: fresh VMs → check detects missing k3s → bootstrap → all green → gen/verify/deploy → doctor healthy → **inject faults (k3s stop, OOMKill) → doctor catches each → recover**.

CI runs gofmt, `go vet` (including under the `integration` build tag, so those files cannot rot unnoticed), race-enabled unit tests, and a cross-compile on every push. The sandbox E2E needs OrbStack VMs, which GitHub-hosted runners cannot provide, so that job targets a self-hosted macOS runner and is skipped elsewhere rather than reported as passing.

Integration tests (`-tags=integration`) read node addresses from `test/sandbox/targets.sandbox.yaml` rather than hardcoding them, because OrbStack assigns new IPs each time the VMs are recreated. Point them at another cluster with `K3HELPER_TARGETS=/path/to/targets.yaml`. With no sandbox running they skip rather than fail.

> Note: sandbox VMs require [OrbStack](https://orbstack.dev) on macOS. Plain Docker containers share the macOS kernel and break kubelet PLEG (pods killed falsely), so real lightweight VMs are used.

## Architecture

```
cmd/k3helper/            entry point
install.sh               one-liner installer (POSIX sh, checksum-verified)
scripts/bundle.sh        offline paste bundle for air-gapped web consoles
internal/
  config/    targets.yaml loading + validation
  ssh/       node transport: SSH client (key auth, run/stream/sudo) or,
             for `local: true` nodes, direct /bin/sh execution
  vm/        k3s bootstrap over SSH (server → token → agents → wait Ready)
  kyaml/     YAML verify (3 layers, offline + live) + generate (12 kinds)
  kube/      cluster reads through kubectl: pods, nodes, events, logs, describe
  sandbox/   locates the test sandbox from targets.sandbox.yaml
  deploy/    dry-run → apply → rollout wait
  check/     check registry: {status, summary, evidence, remediation}
  troubleshoot/  evidence gathering + signature matching + diagnosis ranking
  tui/       Bubble Tea dashboard
  cli/       cobra commands
test/
  sandbox/   OrbStack 3-VM sandbox + setup script
  fixtures/  YAML corpus, goldens
  e2e.sh     full-lifecycle E2E
```

## Known limitations

Current, and worth knowing before pointing this at production:

- **SSH host keys are not verified.** `k3helper` accepts any host key on every connection. On an untrusted network this is exposed to machine-in-the-middle — and `vm setup` sends the cluster join token and pipes an install script to `sudo sh` over that connection. Use it on networks you trust until known-hosts checking lands. (A `local: true` node has no connection to intercept, but every other node in the file still does.)
- **`vm setup` reports "cluster ready" once the *registered* nodes are Ready**, without comparing against the expected node count. An agent that has not registered yet can be missed.
- **The fetched kubeconfig is not rewritten.** `--kubeconfig` copies the server's `k3s.yaml` verbatim, so it still points at `https://127.0.0.1:6443` and won't work from your machine as-is. (It is correct as-is when k3helper runs on the server itself via `local: true`.)
- **Host-layer checks are k3s-specific** (`k3s`/`k3s-agent` systemd units). See the support matrix above.

## Roadmap

- [ ] SSH known-hosts verification, with an explicit opt-out flag
- [ ] Rewrite the fetched kubeconfig's server address to the node's reachable IP
- [ ] `vm setup`: wait for the expected node count, not just the registered ones
- [ ] k8s (kubeadm) host-layer adapter: `kubelet`/`containerd` units, `/etc/kubernetes/admin.conf`
- [ ] More failure signatures (kubelet/containerd health, image GC, clock skew)
- [ ] Fault-injection matrix as `make fault-<name>` targets (disk full, bad token, ImagePull, PVC pending, cordon)

## License

MIT
