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

```bash
git clone https://github.com/solutionforest/k3helper && cd k3helper
make build          # ./bin/k3helper (current platform)
make build-all      # linux/darwin × amd64/arm64 in bin/
```

Requirements: Go 1.22+ to build. At runtime: SSH access to your nodes; `kubectl`/`k3s` on the cluster's server node.

## Quick start

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
Doctor gathers evidence across every layer — VM host (`df`, `free`, cgroups, systemd), the k3s service, node conditions, pod statuses, container states, events — then matches it against a signature knowledge base (ImagePullBackOff, OOMKilled, CrashLoopBackOff, unschedulable/taints, DiskPressure, PVC pending, kubeconfig auth…). Exit code `2` when findings exist.

A node it cannot reach is reported, never skipped — partial inspection must not read as a clean bill of health:

```
✗ node agent2 unreachable: ssh dial sandbox@10.0.0.12:22: dial tcp: i/o timeout

1.  [85%]  Node unreachable over SSH (no evidence could be gathered)
```

### 7. TUI

```bash
k3helper tui -t targets.yaml
```
Dashboard with per-node check cards, severity colors, remediation hints, `r` refresh, `q` quit.

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
```

The E2E script proves the whole loop: fresh VMs → check detects missing k3s → bootstrap → all green → gen/verify/deploy → doctor healthy → **inject faults (k3s stop, OOMKill) → doctor catches each → recover**.

Integration tests (`-tags=integration`) read node addresses from `test/sandbox/targets.sandbox.yaml` rather than hardcoding them, because OrbStack assigns new IPs each time the VMs are recreated. Point them at another cluster with `K3HELPER_TARGETS=/path/to/targets.yaml`. With no sandbox running they skip rather than fail.

> Note: sandbox VMs require [OrbStack](https://orbstack.dev) on macOS. Plain Docker containers share the macOS kernel and break kubelet PLEG (pods killed falsely), so real lightweight VMs are used.

## Architecture

```
cmd/k3helper/            entry point
internal/
  config/    targets.yaml loading + validation
  ssh/       SSH client (key auth, run/stream/sudo)
  vm/        k3s bootstrap over SSH (server → token → agents → wait Ready)
  kyaml/     YAML verify (3 layers, offline + live) + generate (12 kinds)
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

- **SSH host keys are not verified.** `k3helper` accepts any host key on every connection. On an untrusted network this is exposed to machine-in-the-middle — and `vm setup` sends the cluster join token and pipes an install script to `sudo sh` over that connection. Use it on networks you trust until known-hosts checking lands.
- **`vm setup` reports "cluster ready" once the *registered* nodes are Ready**, without comparing against the expected node count. An agent that has not registered yet can be missed.
- **The fetched kubeconfig is not rewritten.** `--kubeconfig` copies the server's `k3s.yaml` verbatim, so it still points at `https://127.0.0.1:6443` and won't work from your machine as-is.
- **Host-layer checks are k3s-specific** (`k3s`/`k3s-agent` systemd units). See the support matrix above.

## Roadmap

- [ ] SSH known-hosts verification, with an explicit opt-out flag
- [ ] Rewrite the fetched kubeconfig's server address to the node's reachable IP
- [ ] `vm setup`: wait for the expected node count, not just the registered ones
- [ ] k8s (kubeadm) host-layer adapter: `kubelet`/`containerd` units, `/etc/kubernetes/admin.conf`
- [ ] More failure signatures (cert expiry, etcd quorum, CoreDNS, service endpoints)
- [ ] Fault-injection matrix as `make fault-<name>` targets (disk full, bad token, ImagePull, PVC pending, cordon)
- [ ] TUI resource browser (pods/logs/events views); live auto-refresh
- [ ] Deploy diff view, multi-cluster contexts
- [ ] GitHub Actions CI running unit + sandbox E2E

## License

MIT
