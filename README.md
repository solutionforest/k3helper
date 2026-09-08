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

Supported kinds: Deployment, StatefulSet, DaemonSet, Service, Ingress, ConfigMap, Secret, PVC, Namespace, Job, CronJob. Generated output always round-trips through `verify` (enforced by tests).

### 5. Deploy

```bash
k3helper deploy -f web.yaml -t targets.yaml
# ✓ applied deployment.apps/web
# ✓ rolled out deployment.apps/web
```
Server-side dry-run validation runs first; `--dry-run` stops there.

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
Doctor gathers evidence across every layer — VM host (`df`, `free`, cgroups, systemd), the k3s service, node conditions, pod statuses, container states, events — then matches it against a signature knowledge base (ImagePullBackOff, OOMKilled, CrashLoopBackOff, unschedulable/taunts, DiskPressure, PVC pending, kubeconfig auth…). Exit code `2` when findings exist.

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
make e2e             # full lifecycle, 16 assertions, ~10-15 min
make e2e-fast        # reuse running sandbox, ~6 min
make test            # unit tests
make test-integration
```

The E2E script proves the whole loop: fresh VMs → check detects missing k3s → bootstrap → all green → gen/verify/deploy → doctor healthy → **inject faults (k3s stop, OOMKill) → doctor catches each → recover**.

> Note: sandbox VMs require [OrbStack](https://orbstack.dev) on macOS. Plain Docker containers share the macOS kernel and break kubelet PLEG (pods killed falsely), so real lightweight VMs are used.

## Architecture

```
cmd/k3helper/            entry point
internal/
  config/    targets.yaml loading + validation
  ssh/       SSH client (key auth, run/stream/sudo)
  vm/        k3s bootstrap over SSH (server → token → agents → wait Ready)
  kyaml/     YAML verify (3 layers) + generate (11 kinds)
  deploy/    dry-run → apply → rollout wait
  check/     check registry: {status, summary, evidence, remediation}
  troubleshoot/  evidence gathering + signature matching + diagnosis ranking
  tui/       Bubble Tea dashboard
  cli/       cobra commands
test/
  sandbox/   OrbStack 3-VM sandbox + setup script
  fixtures/  YAML corpus, goldens
  e2e.sh     full-lifecycle E2E (16 assertions)
```

## Roadmap

- [ ] k8s (kubeadm) host-layer adapter: `kubelet`/`containerd` units, `/etc/kubernetes/admin.conf`
- [ ] More failure signatures (cert expiry, etcd quorum, CoreDNS, service endpoints)
- [ ] TUI resource browser (pods/logs/events views)
- [ ] Deploy diff view, multi-cluster contexts
- [ ] GitHub Actions CI running unit + sandbox E2E

## License

MIT
