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
3. **Schema** — per-kind shape rules offline (Job restartPolicy, DaemonSet
   replicas, container name/image, StatefulSet serviceName…), plus
   `kubectl apply --dry-run=server` against a live cluster via
   `verify --dry-run-server`. Unknown kinds get layers 1–2 only.
Outputs field paths, the line the offending document starts on, and a non-zero
exit code. `--json` for scripts.

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

Five layers. Every feature lands with its tests.

### Layer 1 — Unit tests (no cluster, fast)
- **kyaml**: verify layers 1–3 against a fixture corpus, plus per-kind shape
  rules (a Job with `restartPolicy: Always`, a DaemonSet with `spec.replicas`).
  Round-trip property test: everything `gen` emits must pass `verify`.
- **check**: each check fed synthetic systemd/`df`/`free` output; distro
  detection fed unit-file listings.
- **troubleshoot**: signature matcher fed synthetic evidence bundles; asserts
  ranked root causes, and that a healthy bundle yields *zero* findings.
- **tui**: command bar, filters, drill-down and refresh driven through the
  model without a terminal.
- **ssh**: host key verification — unknown, accept-new, changed — against a
  scratch known_hosts.
- Table-driven, `go test ./...` in seconds, and `-race` in CI.

### Layer 2 — Integration tests (real cluster, `-tags=integration`)
Run against the OrbStack sandbox. Real SSH, real k3s, real kubectl.
Node addresses come from `targets.sandbox.yaml`, never hardcoded — OrbStack
reassigns IPs on every recreate. With no sandbox running they skip rather than
fail. `K3HELPER_TARGETS` points them at another cluster.

- Bootstrap: fresh sandbox → `vm setup` → all nodes Ready.
- Deploy: local manifest → uploaded → applied → rolled out → temp file removed.
- Generator: **every** kind submitted to the live API server via
  `verify --dry-run-server`. The offline rules cannot prove this alone.
- Gatherer: asserts the cluster probes actually return data — a silently
  failing probe is otherwise indistinguishable from a healthy cluster.

### Layer 3 — Fault-injection matrix (the troubleshooter's exam)
`test/faults/fault.sh` injects a fault; `make fault-check-all` injects each in
turn, asserts `doctor` reports the signature that fault should produce, then
cleans up. Matching is on signature IDs from `doctor --json`, not display
titles, so rewording a finding cannot silently break the suite.

| Fault | How | Expected signature |
|---|---|---|
| `k3s-down` | `systemctl stop k3s-agent` | `node.notready-k3s-down` |
| `disk-full` | `fallocate` to fill `/` | `node.diskpressure` |
| `imagepull` | pod with a nonexistent image | `pod.imagepull` |
| `crashloop` | container command exits 1 | `pod.crashloop` |
| `oom` | 10Mi limit, 50MB allocation | `pod.oom` |
| `pending` | request 100 CPU | `pod.pending-sched` |
| `cordon` | cordon every node | `pod.pending-sched` |
| `pvc-pending` | claim on a missing storage class | `storage.pvc-pending` |
| `coredns` | scale CoreDNS to zero | `network.coredns` |
| `empty-endpoints` | Service whose selector matches nothing | `network.empty-endpoints` |
| `bad-kubeconfig` | corrupt `k3s.yaml` | `cluster.kubeconfig` |

Each is `make fault-<name>`, with `make fault-list` and `make fault-clean`.

Verified 2026-09-09: **10 passed, 0 failed.**

> This layer earns its keep. It found that `pod.pending-sched` never matched a
> cordoned node — the remediation said "kubectl uncordon" while no matcher
> looked for `were unschedulable`, and the `0/N nodes are available` literal
> could never fire because the real message carries a live count. It also found
> that deleted PVCs kept reporting "stuck Pending" for an hour, because the
> stale-event filter written for pods was never extended to PVCs.

### Layer 3b — Independent review
Two model-driven reviews (one adversarial, hunting specifically for false
negatives and misdiagnosis) audited the diagnostic paths and found defects the
fault matrix could not, because they require states the sandbox does not
reproduce: a kubeadm node, an API server that is down, a partially-authorised
kubeconfig, an interrupted gather. Findings and fixes are recorded in the
commit history. The classes worth remembering:

- **Symptom outranking cause.** "kubeconfig invalid" scored 85 while the
  stopped k3s that caused it scored 45, so every API-server outage sent the
  user to check a kubeconfig that was fine.
- **Evidence gated on the thing it diagnoses.** Certificate-expiry and etcd
  checks only ran when `kubectl get nodes` succeeded — i.e. never when they
  mattered.
- **Silence read as health.** Probe failures were swallowed, so a partial
  gather printed "no issues detected".
- **Fallbacks that hide the answer.** Chaining kubectl candidates with `||`
  meant a validation error from the real command was replaced by the next
  candidate's "no such file".
- **Stale evidence.** Events outlive their subject by about an hour, so a pod
  that was briefly unschedulable at startup kept producing a finding after it
  recovered.
- **Gathered but never diagnosed.** NotReady nodes were collected and only
  ever used as a correlation bonus, so a NotReady node whose service was still
  running — the commonest serious fault in a cluster — reported nothing at all.
- **Zero values read as measurements.** An unreadable `free` left "0 MB
  available" in the evidence, firing MemoryPressure on a healthy host.

Three rounds were needed: the first fixes introduced their own defects (a
`||` fallback chain that replaced kubectl's real error with the next
candidate's, a CRLF regression in the document splitter), which the second
round found. Reviewing the fixes mattered as much as reviewing the original
code.

### Layer 4 — E2E (`test/e2e.sh`, via `make e2e`)
The whole product against three fresh VMs:

1. Sandbox reset, SSH verified
2. `init` — scaffold, reload, refuse to clobber, `--local`; missing-file error suggests it
3. Host keys — unknown refused, accept-new records, recorded key verifies, **changed key refused**, opt-outs exclusive
4. `check` before install → reports *not installed*, and says to install rather than restart
5. `vm setup` → waits for all three nodes to register **and** go Ready
6. Fetched kubeconfig has no loopback address and **`kubectl` connects with it from this machine**
7. `check` after install → green; `--strict` escalates warnings
8. `gen` → `verify` offline → `verify --dry-run-server`; every kind (incl. kubectl short aliases) accepted by the live API server; an invalid Job caught offline
9. `deploy` ships the local manifest itself and leaves no temp files
10. `deploy --diff`, `--namespace` (missing, conflicting, correct)
11. Multi-cluster `ctx` / `--context`
12. `doctor` healthy + `--json`; an unreachable node is never a clean bill of health
13. Fault → detect → recover
14. Fault sweep across six signatures
15. Cleanup → healthy

Modes: `make e2e` (fresh sandbox, ~12 min), `make e2e-fast` (`--keep`),
`make e2e-quick` (`--keep --quick`, skips the sweep).

Verified 2026-09-09: **67 passed, 0 failed.**

### Layer 5 — CI
GitHub Actions on every push: gofmt, `go vet`, `go vet -tags=integration`
(those files are never compiled by a plain vet and rot silently otherwise),
`go test -race`, and a cross-compile.

The sandbox E2E needs OrbStack VMs, which GitHub-hosted runners cannot
provide, so that job targets a self-hosted macOS runner and is **skipped**
elsewhere rather than reported as passing.

## OrbStack sandbox — three real VMs

`test/sandbox/`: three OrbStack Linux VMs that behave like SSH-reachable hosts.

> Two drivers exist. `setup-orbstack.sh` creates VMs and is what the recorded
> results were produced on. `setup-docker.sh` creates three privileged systemd
> containers for hosts that cannot nest virtualisation.
>
> The container driver provisions correctly and k3s reaches Ready, but pod
> networking does not work there: pods get addresses from the flannel range
> that nothing can reach, so readiness probes fail and CoreDNS crashloops.
> Mounting /lib/modules fixed one real blocker — k3s could not modprobe the
> iptables/nftables modules it needs — but the remaining problem is
> container-in-container CNI networking, which likely needs the approach k3d
> takes (purpose-built images and networking). That is why the CI E2E job is
> dispatch-only rather than running on every push.

- `setup-orbstack.sh` creates the VMs, installs sshd, provisions the `sandbox`
  user with passwordless sudo, and writes `targets.sandbox.yaml` with live IPs.
  It asserts the key landed rather than trusting `orb`, which does not
  propagate the guest command's exit status.
- k3s is installed by *our* bootstrap code over SSH — the image never ships it,
  because installing it is what we are testing.
- `insecure_host_key: true` in the generated targets: these VMs get a new IP
  and host key on every recreate.

## Project layout
```
cmd/k3helper/main.go
internal/
  cli/          cobra commands (init, ctx, check, doctor, deploy, gen, verify, vm, tui)
  config/       targets file: single- and multi-cluster, validation
  ssh/          SSH + local transport, host key verification, file transfer
  vm/           k3s bootstrap over SSH, kubeconfig fetch/rewrite
  kyaml/        YAML verify (3 layers, offline + live) + generate (12 kinds)
  kube/         cluster reads via kubectl: pods, nodes, events, logs, describe
  check/        check registry + per-distribution host layer (k3s / kubeadm)
  troubleshoot/ evidence gathering, signature matching, ranked diagnosis
  deploy/       dry-run → diff → apply → rollout wait
  tui/          Bubble Tea dashboard + resource browser
  sandbox/      locates the test sandbox for integration tests
test/
  sandbox/      OrbStack VM provisioning + targets.sandbox.yaml
  faults/       fault.sh (inject) + check-all.sh (the exam)
  fixtures/     yaml corpus
  e2e.sh        full-lifecycle E2E
install.sh      release installer
scripts/bundle.sh
```

## Delivery status
0. **Sandbox** — three OrbStack VMs, `make sandbox-up`. ✅
1. **Scaffold** — cobra CLI, config loading, version. ✅
2. **YAML verify + generate** — fixture corpus, per-kind rules, live dry-run. ✅
3. **Check engine** — synthetic fixtures; k3s and kubeadm host layers. ✅
4. **VM setup over SSH** — bootstrap from scratch, waits for the full node count. ✅
5. **Troubleshoot engine** — 15 signatures, fault matrix green. ✅
6. **Deploy** — upload, dry-run, diff, namespace handling, rollout wait. ✅
7. **TUI** — dashboard + resource browser (pods/nodes/events/logs/describe). ✅
8. **Polish + release** — cross-compile, installer, README, CI. ✅

Remaining: HA control plane (multiple servers, embedded etcd), a Linux CI
sandbox so the E2E can run on GitHub-hosted runners, and the TUI extras
(port-forward manager, multi-pod log tailing).

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

## Resolved questions
1. **Binary name** — `k3helper`. Settled.
2. **Offline schema validation** — neither extreme. `verify` ships hand-written
   per-kind rules for the kinds we generate (cheap, no embedded schema blob)
   and defers full schema checking to the live API server via
   `--dry-run-server`. Unknown kinds and CRDs get structural checks only rather
   than guessed-at rules. Embedding kubeconform schemas remains an option if
   offline full validation is ever needed.
