# k3s/k8s helper — plan

**One Go binary** (`k3helper`) with a full-feature Bubble Tea TUI + non-interactive subcommands. Two operating modes: on-node (local `kubectl`/k3s) and remote (SSH into nodes).

## Requirements
1. Portable
2. As simple as possible
3. Verify the yaml
4. Generate correct yaml
5. Setup the vm machine
6. Troubleshoot the vm machine / k8s / k3s (most important)

## Scope decision
**Full feature.** We do not delegate to k9s or drop any layer. k3helper covers the complete lifecycle: VM → k3s install → yaml verify/generate → deploy → monitor → troubleshoot with remediation. The TUI includes its own resource browser (nodes, pods, logs, events) — deliberately simpler than k9s, but present, so the tool is self-contained on a jump host with nothing but ssh + this binary.

## Design principles (#1 + #2)
- Single static binary, cross-compiled (linux/amd64, linux/arm64, darwin/arm64). No runtime deps.
- Shell out to `kubectl` for cluster work (handles auth/contexts robustly); SSH via `x/crypto/ssh`. No client-go auth plugin complexity.
- Config is one YAML file listing clusters + nodes + providers.
- Every capability is a plain CLI command first, then wrapped in the TUI — keeps things simple and lets scripts/CI use it.

## Core libraries
`bubbletea` + `bubbles` + `lipgloss` (TUI), `sigs.k8s.io/yaml` (k8s-style YAML), `x/crypto/ssh` (SSH), `spf13/cobra` (CLI), `go-testdeep` or stdlib for test assertions, `docker compose` for the test sandbox.

## Modules

### 1. YAML verify (#3)
Layered, offline-first:
1. **Syntax** — `sigs.k8s.io/yaml` parse.
2. **Structural** — required fields (`apiVersion`, `kind`, `metadata.name`).
3. **Schema** — `kubectl apply --dry-run=server -f` when a cluster is reachable; offline schema validation (kubeconform-style embedded schemas) as fallback.
Outputs precise line/field errors + exit code.

### 2. YAML generate (#4)
`k3helper gen <kind>` for the common kinds (Deployment, Service, Ingress, ConfigMap, Secret, PVC, Namespace, StatefulSet, Job, CronJob, k3s-specific like IngressRoute). Interactive prompt flow in TUI; `--flags` for scripts. Emits valid, schema-correct YAML with current `apiVersion`s. Round-trip guarantee: everything `gen` emits must pass `verify`.

### 3. VM setup (#5) — providers: SSH, Multipass, Vagrant, Cloud
- **SSH** (existing boxes): read `targets.yaml`, install k3s server + agents over SSH (token, cni, taint options), verify join.
- **Multipass**: `multipass launch` → bootstrap.
- **Vagrant**: generate `Vagrantfile` + provision → bootstrap.
- **Cloud**: generate cloud-init/user-data + orchestrate via provider CLI (`aws`/`gcloud`/`doctl`) — no SDKs, to stay simple.
- Shared `bootstrap.sh` template as the single source of truth for k3s install (k3sup-proven pattern).

### 4. Quick deploy
`k3helper deploy -f <file>` → dry-run → confirm → apply → wait for rollout → report.

### 5. Check engine
Registry of checks returning `{status, summary, details, remediation}`:
- **Cluster**: nodes Ready, control plane, scheduler/controller, apiserver reachable.
- **Node**: disk/inode/memory/PID pressure, swap, kernel modules, time sync, unique hostname.
- **k3s**: `k3s` service state, containerd, server/agent role, token, CNI/flannel, cert expiry.
- **Workloads**: pod not-ready reasons, CrashLoopBackOff, ImagePullBackOff, OOMKilled, Pending (scheduling).
- **Networking/Storage**: CoreDNS, endpoints, PVC pending, CSI.

### 6. Troubleshoot (#6 — highest priority)
Two tiers:
- **`k3helper doctor`** — runs all checks, produces a ranked report with remediation per finding.
- **Signature matcher** — maps gathered evidence to known failure signatures (e.g. "ImagePullBackOff + private registry" → auth/credential fix; "OOMKilled" → limits; "Pending + no nodes" → taints). Ships with a built-in knowledge base of common k3s failure patterns.

### 7. TUI (full feature — k9s-style, but fancier)
A resource browser in the k9s mold (type `:` + alias to jump between resource views) extended with our exclusive capabilities. Fancy = modern styling + live data + drill-down everywhere.

**Interaction model (k9s parity)**
- Command bar: `:pods⏎`, `:nodes⏎`, `:deploy⏎` — singular/plural/short-name aliases
- `/filter⏎` regex filter, `-l label` selector, fuzzy find
- Auto-refreshing live tables (bubbles `table`, 2s tick), breadcrumb crumbs, keyboard-first with mouse fallback
- Sort columns (`shift-n/a/s`), wide mode (`ctrl-w`), toggle faults view (`ctrl-z`)

**Views**
| Key | View | Extras beyond k9s |
|---|---|---|
| dashboard | Cluster overview: node cards, health score ring, alert feed, sparkline CPU/mem trends | Health score computed from our check engine |
| `:po` `:no` `:dp` `:svc` `:ing` `:sts` `:ds` `:ev` ... | Resource tables with drill-down (describe / logs streaming / yaml / events) | Every row shows k9s columns + our check verdict icon |
| `:doctor` | Run all checks live; findings table with severity | Jump finding → evidence → **one-keystroke remediation hint** |
| `:deploy` | Pick file → dry-run diff side-by-side → apply → live rollout progress | Diff view is ours, k9s has none |
| `:gen` | YAML Studio: prompt flow → live preview → verify inline → save | Inline error highlighting with line numbers |
| `:vm` | Targets list from `targets.yaml`, bootstrap progress per node | Live SSH streaming output |
| `:ctx` `:ns` | Context/namespace switcher | — |
| `:logs` | Multi-pod log tailing with color + filter | — |
| `:xray` | Ownership graph (deploy→rs→pod), tree view | — |
| `:ports` | Port-forward manager | — |

**Fancy factor (lipgloss/glamour)**
- Themeable skins (YAML skin files like k9s, shipped with 3: dark/light/k3s-orange)
- Styled panes with rounded borders, card grid on dashboard, colored severity badges
- Sparkline graphs (bubbles `sparkline`) for node CPU/mem in dashboard + node drill-down
- Progress bars (bubbles `progress`) during bootstrap/deploy/rollout
- Streaming logs with syntax-aware coloring; glamour-rendered `describe`/yaml detail panes
- Status header bar: context, namespace, cluster score, fault count — always visible

**Tech:** bubbletea + bubbles (table/list/textinput/spinner/progress/sparkline/viewport) + lipgloss styling + glamour markdown/yaml rendering. Live data via a poll loop (`kubectl get -w` style) feeding shared state; SSH streams multiplexed into the same update channel.

## Testing strategy

Four layers; every feature lands with its tests.

### Layer 1 — Unit tests (no cluster, fast)
- **yaml**: verify layer 1/2/3 against a fixture corpus (`good/`, `bad-syntax/`, `bad-structure/`, `bad-schema/`).
- **gen**: golden-file tests — `gen deployment ...` output must byte-match goldens AND pass `verify` (round-trip property test).
- **check**: each check given synthetic `kubectl`/SSH output fixtures (JSON stdin), assert pass/warn/fail + remediation text.
- **troubleshoot**: signature matcher fed synthetic evidence bundles, assert ranked root causes.
- Table-driven, `go test ./...` in seconds.

### Layer 2 — Integration tests (real cluster, gated by `-tags=integration`)
- Run against the Docker sandbox (below). Real SSH, real k3s, real kubectl.
- Bootstrap flow test: fresh sandbox → `vm setup` → `check` passes all.
- Deploy flow test: `gen` → `verify` → `deploy` → pod Running → rollout report.
- Doctor smoke: healthy cluster ⇒ zero critical findings.

### Layer 3 — Fault-injection tests (the troubleshooter's exam)
Inject each failure into the sandbox, run `doctor`, assert the expected signature + remediation fires:

| Injected fault | How | Expected signature |
|---|---|---|
| k3s service down | `systemctl stop k3s` on a node | k3s service check fails, node NotReady |
| Disk full | `fallocate` a big file in container | DiskPressure / node check fail |
| Wrong join token | join agent with bad token | agent fails to join; check reports |
| ImagePullBackOff | deploy with nonexistent image | workload signature → wrong image/registry |
| CrashLoopBackOff | bad container command | workload signature → app crash, point at logs |
| OOMKilled | memory limit 16Mi + allocation | OOM signature → raise limits |
| Pending pod | request 100 CPU | scheduling signature → insufficient resources |
| PVC pending | claim with missing storage class | storage signature |
| Node cordoned | `kubectl cordon` | scheduling signature → node unschedulable |
| Bad kubeconfig | corrupt config file | config/auth signature |

Each is a Makefile target (`make fault-disk-full`, `make fault-clean`) so tests and humans can both run them.

### Layer 4 — TUI tests
- `teatest` (charmbracelet's Bubble Tea test harness): send keys, assert views render, flows complete (e.g., gen prompt → output file created).

### Layer 5 — Full-lifecycle E2E (`test/e2e.sh`, run via `make e2e`)
The screen-demo saved as a repeatable script. Two modes:
- `make e2e` — resets the sandbox from scratch, then runs the full lifecycle (~10-15 min)
- `make e2e-fast` (`--keep`) — reuses the running sandbox (~6 min)

Steps (16 assertions):
1. Sandbox up, SSH verified
2. `check` BEFORE install → k3s reported missing
3. `vm setup` bootstrap → cluster ready
4. `check` AFTER install → all green
5. `gen` → `verify` → `deploy` → pod Running
6. `doctor` on healthy cluster → zero findings, exit 0
7. FAULT: stop k3s-agent → `doctor` catches at 90%, exit 2
8. RECOVER → doctor healthy again
9. FAULT: OOMKill pod (10Mi limit, 50MB alloc) → `doctor` catches at 95%
10. Cleanup → doctor healthy

Verified 2026-09-08: **16 passed, 0 failed.**

## Docker sandbox — simulating 3 VMs

`test/sandbox/`: three containers that behave like SSH-reachable Linux VMs.

```
docker-compose.yml:
  sandbox-server  → ubuntu:24.04 + openssh-server + systemd, privileged, port 2221→22
  sandbox-agent1  → same, port 2222→22
  sandbox-agent2  → same, port 2223→22
```

> Base image: `ubuntu:24.04` LTS (systemd support, matches production k3s hosts). `ubuntu:26.04` is a drop-in swap via build arg once validated — keep the image pinned through `ARG UBUNTU_VERSION=24.04` so the sandbox matrix can test both.

- **Privileged + systemd entrypoint** so `systemctl status k3s`, cgroups, and kernel-module checks work realistically inside containers.
- Passwordless sudo + shared test SSH key baked in (localhost-only).
- `targets.sandbox.yaml` checked into repo pointing k3helper at `localhost:2221-2223`.
- k3s installed by OUR bootstrap code over SSH (dogfooding — the sandbox is never pre-installed with k3s by the image; that's what we're testing).
- Resource-light: containers with 1-2GB limits run a 3-node k3s cluster fine on a laptop.

Makefile:
```
make sandbox-up      # start 3 VMs
make sandbox-down    # stop + remove
make sandbox-reset   # fresh state (for bootstrap tests)
make test            # unit tests
make test-integration# unit + integration (-tags=integration, needs sandbox)
make fault-<name>    # inject fault N
make fault-clean     # remove all faults
```

CI: GitHub Actions runs unit tests on every push; integration job boots the sandbox, runs bootstrap + fault-injection suite.

## Project layout
```
cmd/k3helper/main.go
internal/{tui,cli,check,troubleshoot,yaml,deploy,vm,kube,config}
test/
  sandbox/          # docker-compose.yml, Dockerfile, targets.sandbox.yaml
  faults/           # fault-injection scripts
  fixtures/         # yaml corpus, command output fixtures, goldens
targets.example.yaml
```

## Phased delivery (each ends in something usable + tested)
0. **Sandbox** — docker-compose 3 VMs, SSH working, `make sandbox-up` verified. (Built first: everything after is tested against it.)
1. **Scaffold** — go.mod, cobra CLI, config loading, version. Unit tests for config.
2. **YAML verify + generate** — fixture corpus + golden + round-trip tests.
3. **Check engine** — synthetic fixture tests.
4. **VM setup over SSH** — integration test: bootstrap sandbox from scratch, `check` green.
5. **Troubleshoot engine** — fault-injection suite green for the full matrix above.
6. **Deploy** — integration test: gen→verify→deploy→running.
7. **TUI** — teatest interaction tests + manual QA against sandbox.
8. **Polish + release** — cross-compile Makefile, README, demo recording.

## Research — similar tools

| Tool | What it does | Covers our reqs | Gap |
|------|--------------|-----------------|-----|
| **k9s** (derailed/k9s) | k8s TUI: browse/observe/manage live cluster, logs, describe, edit, port-forward | TUI only | No YAML gen/verify, no VM setup, no host/k3s-level diagnostics, no guided troubleshooting |
| **k3sup** (alexellis/k3sup) | Install k3s on any VM over SSH, join agents, fetch kubeconfig, HA | #5 VM setup (SSH) | Pure CLI, no TUI, no YAML, no health checks beyond `k3sup ready` |
| **Popeye** (derailed/popeye) | Live cluster linter; misconfig, stale resources, resource alloc | #6 partial | Resource-level only (no VM/host/k3s service), emits codes not remediation, read-only |
| **kubeconform** (yannh/kubeconform) | Validate k8s YAML against JSON schemas, offline | #3 verify | No generation, no cluster/host access, no troubleshooting (kubeval deprecated) |
| **k3d** (k3d-io/k3d) | k3s-in-docker, local multi-node clusters | quick local deploy | No VM provisioning, no troubleshooting, no YAML |
| **kubectl built-ins** | `create --dry-run=client -o yaml`, `apply --dry-run=server`, `debug`/`describe`/`logs` | pieces of #3/#4 | No VM/host layer, no TUI, no aggregation |
| **k3s built-ins** | `k3s check-config`, `k3s ctr`, `k3s kubectl`, `etcd-snapshot` | #6 partial | Scattered; no aggregation, no remediation |
| **AutoK3s** (cnrancher/autok3s) | Run k3s everywhere incl. cloud | #5 cloud | Web/CLI, heavyweight, no TUI/troubleshoot |
| **Others** | stern (logs), kubectl diagnose/neat (krew), Robusta, Lens/Rancher | partial | Each does one narrow slice |

Maintenance status (checked 2026-09-07): k9s v0.51.0 (Jun 2026) and k3sup (Sep 2026) actively maintained; k3d v5.9.0 (Jun 2026) active; popeye v0.22.1 (Jan 2025) slowing; autok3s v0.9.3 (Jul 2024) stale; kubeval dead since 2021.

### Key observations
- **k9s** stops at "observe/manage" — never inspects the host OS, the `k3s` systemd service, or proposes fixes.
- **k3sup** nails "VM → kubeconfig in 60s" but troubleshooting is a README appendix, not a feature.
- **Popeye** can't tell you the disk is full, kernel modules are missing, or `k3s` crashed on boot.
- **kubeconform** already solves offline YAML validation; wrap/embed rather than reinvent.
- **k3s** ships `k3s check-config`; wrap it, don't reimplement.

### Positioning
No existing tool spans **VM host layer → k3s service → cluster → workloads → YAML** in one portable binary with a full TUI. The moat is the **guided troubleshooting** (`doctor`) that reasons across *all* layers and ranks root causes with concrete remediation — validated by the fault-injection test suite, which no comparable project ships.

### Implications for our build
- **Leverage:** wrap `kubectl`, `k3s check-config`, kubeconform-style schemas, k3sup's SSH bootstrap pattern.
- **Concentrate effort on:** (1) cross-layer `doctor`/signature engine, (2) YAML generator, (3) unified TUI, (4) the fault-injection test suite that keeps the troubleshooter honest.

## Open questions
1. Binary name — `k3helper` ok, or prefer something else?
2. Offline schema validation: embed kubeconform schemas (bigger binary, full validation) vs structural checks only (tiny binary)? Leaning: embed — portability means we can't assume kubectl/network everywhere.
