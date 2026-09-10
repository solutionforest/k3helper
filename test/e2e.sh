#!/bin/bash
# =============================================================================
# k3helper E2E — the whole product, exercised against the OrbStack sandbox.
#
#   sandbox reset → init → host keys → check(no k3s) → bootstrap → kubeconfig
#   → check(green) → gen → verify(offline+live) → deploy → diff → namespaces
#   → contexts → doctor(healthy) → fault sweep → recover → cleanup
#
# Usage:
#   ./test/e2e.sh                 full run incl. sandbox reset (~12 min)
#   ./test/e2e.sh --keep          skip the reset, reuse a running sandbox
#   ./test/e2e.sh --keep --quick  skip the multi-fault sweep too
#
# Exit code 0 = every assertion passed.
# =============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
TARGETS=test/sandbox/targets.sandbox.yaml
KEEP=0; QUICK=0
for a in "$@"; do
  case "$a" in
    --keep)  KEEP=1 ;;
    --quick) QUICK=1 ;;
    *) echo "unknown flag: $a"; exit 2 ;;
  esac
done

PASS=0; FAIL=0
FAILED_NAMES=()
step() { echo; echo "━━━ $* ━━━"; }
ok()   { echo "  ✓ $1"; PASS=$((PASS+1)); }
bad()  { echo "  ✗ $1"; FAILED_NAMES+=("$1"); FAIL=$((FAIL+1)); }

assert_contains() { # desc haystack needle
  if printf '%s' "$2" | grep -qF -- "$3"; then ok "$1"
  else bad "$1 — expected '$3' in:"; printf '%s' "$2" | sed 's/^/      /' | head -6; fi
}
assert_not_contains() { # desc haystack needle
  if printf '%s' "$2" | grep -qF -- "$3"; then bad "$1 — did NOT expect '$3' in:"; printf '%s' "$2" | sed 's/^/      /' | head -6
  else ok "$1"; fi
}
assert_matches() { # desc haystack extended-regex
  if printf '%s' "$2" | grep -qE -- "$3"; then ok "$1"
  else bad "$1 — expected match /$3/ in:"; printf '%s' "$2" | sed 's/^/      /' | head -6; fi
}
assert_exit() { # desc expected actual
  if [ "$2" = "$3" ]; then ok "$1 (exit=$3)"; else bad "$1 — expected exit $2, got $3"; fi
}

# always rebuild: a stale bin/ would silently test a different revision
echo "building..."
go build -o bin/k3helper ./cmd/k3helper || exit 1
K=bin/k3helper

# ── 0. sandbox ────────────────────────────────────────────────────────────────
if [ "$KEEP" = 0 ]; then
  step "0. Reset sandbox (3 fresh Ubuntu 24.04 VMs)"
  # go through make so the OrbStack/Docker driver choice is made in one place
  make sandbox-down >/dev/null 2>&1
  if ! make sandbox-up > /tmp/e2e-sandbox.log 2>&1; then
    echo "sandbox setup failed:"; tail -12 /tmp/e2e-sandbox.log; exit 1
  fi
  grep -q "sandbox ready" /tmp/e2e-sandbox.log && ok "3 sandbox hosts up, SSH verified" \
    || bad "sandbox did not report ready"
fi

read_host() { grep -A5 "name: $1\$" "$TARGETS" | grep 'host:' | head -1 | awk '{print $2}'; }
SERVER_IP=$(read_host server)
AGENT1_IP=$(read_host agent1)
SSHO=(-i test/sandbox/ssh/id_ed25519 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8 -o BatchMode=yes)
kctl() { ssh "${SSHO[@]}" "sandbox@$SERVER_IP" "sudo k3s kubectl $*" 2>/dev/null; }

# ── 1. init: build a targets file from nothing ───────────────────────────────
step "1. init — scaffold a targets file"
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
OUT=$($K init --server "$SERVER_IP" --agent "$AGENT1_IP" --user sandbox \
        --key "$ROOT/test/sandbox/ssh/id_ed25519" --insecure-host-key \
        -o "$WORK/targets.yaml" 2>&1)
assert_contains "init writes a targets file" "$OUT" "wrote $WORK/targets.yaml"
OUT=$($K ctx -t "$WORK/targets.yaml" 2>&1)
# ctx prints CLUSTER, REACHED, NODES: an SSH cluster with two nodes.
assert_matches "the generated file loads back" "$OUT" "my-cluster +ssh +2"
OUT=$($K init -o "$WORK/targets.yaml" 2>&1); RC=$?
assert_exit "init refuses to clobber an existing file" 1 $RC
OUT=$($K init --local -o "$WORK/local.yaml" 2>&1)
assert_contains "init --local describes this machine" "$OUT" "this machine (local)"

# a missing targets file must explain itself, not just fail to open
OUT=$($K check -t "$WORK/definitely-absent.yaml" 2>&1)
assert_contains "missing targets file suggests init" "$OUT" "k3helper init"

# ── 2. SSH host key verification ─────────────────────────────────────────────
step "2. SSH host key verification"
grep -v 'insecure_host_key' "$TARGETS" > "$WORK/strict.yaml"
KH="$WORK/known_hosts"
OUT=$(HOME="$WORK" $K check -t "$WORK/strict.yaml" 2>&1)
assert_contains "unknown host is refused by default" "$OUT" "is not in known_hosts"
assert_contains "refusal shows the fingerprint" "$OUT" "SHA256:"
assert_contains "refusal offers a way forward" "$OUT" "--accept-new-host-key"

OUT=$(HOME="$WORK" $K check -t "$WORK/strict.yaml" --accept-new-host-key 2>&1)
assert_not_contains "--accept-new-host-key connects" "$OUT" "not in known_hosts"
[ -s "$WORK/.ssh/known_hosts" ] && ok "the accepted key was recorded" || bad "known_hosts was not written"
OUT=$(HOME="$WORK" $K check -t "$WORK/strict.yaml" 2>&1)
assert_not_contains "the recorded key then verifies without the flag" "$OUT" "not in known_hosts"

# A changed key must be refused even in accept-new mode. The substituted key
# has to be a *valid* different key: a mangled line is skipped as unparseable,
# which makes the host look unknown rather than changed.
ssh-keygen -q -t ed25519 -N '' -f "$WORK/other" <<<y >/dev/null 2>&1
OTHER_KEY=$(awk '{print $1" "$2}' "$WORK/other.pub")
awk -v k="$OTHER_KEY" '{print $1" "k}' "$WORK/.ssh/known_hosts" > "$WORK/.ssh/known_hosts.new"
mv "$WORK/.ssh/known_hosts.new" "$WORK/.ssh/known_hosts"
OUT=$(HOME="$WORK" $K check -t "$WORK/strict.yaml" --accept-new-host-key 2>&1)
# Either wording is a refusal: a same-algorithm swap reads as CHANGED, a
# different-algorithm one as "known, but not under the key type it offered".
# What matters is that accept-new does not silently trust it.
assert_matches "a substituted host key is refused even with --accept-new" "$OUT" "CHANGED|not under the key type"
assert_not_contains "and the node is not reported as healthy" "$OUT" "disk 4"
rm -rf "$WORK/.ssh"

OUT=$($K check -t "$WORK/strict.yaml" --insecure-host-key --accept-new-host-key 2>&1)
assert_contains "the two opt-outs are mutually exclusive" "$OUT" "mutually exclusive"

# ── 3. check BEFORE install ──────────────────────────────────────────────────
if [ "$KEEP" = 0 ]; then
  step "3. check BEFORE install — k3s must be reported missing, with the right fix"
  OUT=$($K check -t $TARGETS 2>&1)
  assert_contains "no Kubernetes service reported on a bare node" "$OUT" "no Kubernetes service found"
  # the whole point of the fix: don't tell someone to restart a service that
  # was never installed
  assert_contains "remediation says to install it" "$OUT" "vm setup"
  assert_not_contains "remediation does not say 'restart'" "$OUT" "systemctl restart"
fi

# ── 4. bootstrap ─────────────────────────────────────────────────────────────
step "4. Bootstrap k3s on all 3 nodes over SSH"
rm -f "$WORK/kubeconfig"
OUT=$($K vm setup -t $TARGETS \
  --server-extra-args "--snapshotter=native --disable=traefik" \
  --agent-extra-args "--snapshotter=native" \
  --kubeconfig "$WORK/kubeconfig" 2>&1)
assert_contains "cluster ready" "$OUT" "cluster ready"
assert_matches "waited for all 3 nodes, not just the registered ones" "$OUT" "waiting for 3 node"

# ── 5. the fetched kubeconfig must work from HERE ────────────────────────────
step "5. Fetched kubeconfig is usable from this machine"
if [ -f "$WORK/kubeconfig" ]; then
  ok "kubeconfig written"
  assert_not_contains "loopback address was rewritten" "$(cat "$WORK/kubeconfig")" "127.0.0.1"
  assert_contains "points at the real server address" "$(cat "$WORK/kubeconfig")" "$SERVER_IP"
  if command -v kubectl >/dev/null 2>&1; then
    OUT=$(KUBECONFIG="$WORK/kubeconfig" kubectl get nodes --request-timeout=15s 2>&1)
    assert_contains "kubectl reaches the cluster with it" "$OUT" "Ready"
  else
    echo "  - kubectl not installed locally; skipping the live connection check"
  fi
else
  bad "kubeconfig was not written"
fi

# ── 6. check AFTER install ───────────────────────────────────────────────────
step "6. check AFTER install — all green"
sleep 10
OUT=$($K check -t $TARGETS 2>&1)
assert_contains "server k3s active" "$OUT" "k3s services active (k3s=active)"
assert_contains "agent k3s-agent active" "$OUT" "k3s services active (k3s-agent=active)"
assert_matches "no disk pressure" "$OUT" "disk [0-9]+% used"
$K check -t $TARGETS >/dev/null 2>&1; assert_exit "check exits 0 when only warnings are present" 0 $?
# the sandbox has swap on, which is a warning; --strict must escalate it
OUT=$($K check -t $TARGETS 2>&1)
if printf '%s' "$OUT" | grep -q "WARN"; then
  $K check -t $TARGETS --strict >/dev/null 2>&1; assert_exit "--strict escalates warnings to a failure" 2 $?
else
  echo "  - no warnings on this cluster; skipping the --strict check"
fi

# ── 7. gen → verify → deploy ─────────────────────────────────────────────────
step "7. gen → verify (offline + live) → deploy"
$K gen deployment e2e-web -i nginx:alpine -r 1 -p 80 -o "$WORK/web.yaml"
OUT=$($K verify "$WORK/web.yaml" 2>&1)
assert_contains "generated manifest verifies offline" "$OUT" "Deployment/e2e-web"
OUT=$($K verify "$WORK/web.yaml" --dry-run-server -t $TARGETS 2>&1)
assert_contains "and against the live API server" "$OUT" "Deployment/e2e-web"

# every kind gen emits must be accepted by a real API server
GENFAIL=""
for kind in namespace configmap secret service pod pvc deploy sts ds job cj ing; do
  $K gen "$kind" "e2e-$kind" -i busybox:latest -o "$WORK/k.yaml" 2>/dev/null || { GENFAIL="$GENFAIL $kind(gen)"; continue; }
  $K verify "$WORK/k.yaml" --dry-run-server -t $TARGETS >/dev/null 2>&1 || GENFAIL="$GENFAIL $kind"
done
[ -z "$GENFAIL" ] && ok "every kind (incl. kubectl short aliases) is accepted by the API server" \
                  || bad "API server rejected generated:$GENFAIL"

# a manifest the offline rules should catch, without needing a cluster
cat > "$WORK/bad.yaml" <<'EOF'
apiVersion: batch/v1
kind: Job
metadata:
  name: e2e-bad
spec:
  template:
    spec:
      containers:
        - name: c
          image: busybox:latest
EOF
OUT=$($K verify "$WORK/bad.yaml" 2>&1); RC=$?
assert_exit "an invalid Job fails verification offline" 1 $RC
assert_contains "and says which field is wrong" "$OUT" "restartPolicy"

# deploy ships the local manifest itself — no manual scp
OUT=$($K deploy -f "$WORK/web.yaml" -t $TARGETS 2>&1)
assert_contains "applied" "$OUT" "applied deployment.apps/e2e-web"
assert_contains "rolled out" "$OUT" "rolled out deployment.apps/e2e-web"
PODS=$(kctl get pods -l app=e2e-web --no-headers 2>/dev/null | grep -c Running)
[ "${PODS:-0}" -ge 1 ] && ok "pod Running in cluster" || bad "pod not running"
# and cleans up after itself
LEFT=$(ssh "${SSHO[@]}" "sandbox@$SERVER_IP" 'ls /tmp/k3helper-* 2>/dev/null | wc -l' 2>/dev/null)
[ "${LEFT:-0}" = "0" ] && ok "no temp manifests left on the server" || bad "deploy left $LEFT temp file(s) behind"

# ── 8. deploy --diff ─────────────────────────────────────────────────────────
step "8. deploy --diff against live state"
OUT=$($K deploy -f "$WORK/web.yaml" -t $TARGETS --diff --dry-run 2>&1)
assert_contains "unchanged manifest reports no changes" "$OUT" "no changes against live cluster state"
$K gen deployment e2e-web -i nginx:1.25-alpine -r 2 -p 80 -o "$WORK/web2.yaml"
OUT=$($K deploy -f "$WORK/web2.yaml" -t $TARGETS --diff --dry-run 2>&1)
assert_contains "a changed image shows in the diff" "$OUT" "nginx:1.25-alpine"
assert_matches "a changed replica count shows too" "$OUT" '\+  replicas: 2'
$K deploy -f "$WORK/web2.yaml" -t $TARGETS >/dev/null 2>&1; assert_exit "--dry-run changed nothing, so the real apply works" 0 $?

# ── 9. namespaces ────────────────────────────────────────────────────────────
step "9. deploy --namespace"
OUT=$($K deploy -f "$WORK/web.yaml" -t $TARGETS -n e2e-missing-ns 2>&1); RC=$?
assert_exit "a namespace that does not exist fails" 1 $RC
assert_matches "and says so" "$OUT" "not found|NotFound"
kctl create namespace e2e-ns >/dev/null 2>&1
OUT=$($K deploy -f "$WORK/web.yaml" -t $TARGETS -n e2e-ns 2>&1)
assert_contains "deploys into the requested namespace" "$OUT" "rolled out"
LANDED=$(kctl get deploy e2e-web -n e2e-ns --no-headers 2>/dev/null | wc -l | tr -d ' ')
[ "$LANDED" = "1" ] && ok "the resource really landed in e2e-ns" || bad "resource is not in e2e-ns"
printf 'apiVersion: v1\nkind: Service\nmetadata:\n  name: e2e-conflict\n  namespace: prod\nspec:\n  ports:\n    - port: 80\n' > "$WORK/conflict.yaml"
OUT=$($K deploy -f "$WORK/conflict.yaml" -t $TARGETS -n e2e-ns 2>&1)
assert_contains "a namespace conflict is refused, not silently redirected" "$OUT" "conflicts with the manifest"

# ── 10. contexts ─────────────────────────────────────────────────────────────
step "10. Multi-cluster contexts"
{
  echo "clusters:"
  echo "  - cluster: sandbox"
  echo "    nodes:"
  # re-indent the sandbox node list by four spaces to sit under this entry
  sed -n '/^nodes:/,$p' "$TARGETS" | sed '1d' | sed 's/^/    /'
  echo "  - cluster: elsewhere"
  echo "    nodes:"
  echo "      - name: server"
  echo "        role: server"
  echo "        host: 10.255.255.1"
  echo "        user: nobody"
  echo "        key: $ROOT/test/sandbox/ssh/id_ed25519"
  echo "        insecure_host_key: true"
  echo "current: sandbox"
} > "$WORK/multi.yaml"
OUT=$($K ctx -t "$WORK/multi.yaml" 2>&1)
assert_contains "ctx lists both clusters" "$OUT" "elsewhere"
assert_matches "and marks the current one" "$OUT" '\* +sandbox'
# Assert which cluster the default context resolved to, not that it is
# healthy: this step runs while earlier steps' resources are still being torn
# down, and cluster health is asserted properly in step 11.
DOC=$($K doctor -t "$WORK/multi.yaml" --json 2>&1) || true
assert_not_contains "default context reached the sandbox, not the unreachable cluster" "$DOC" "node.unreachable"
# With the *server* unreachable there is no kubectl to gather through, so
# doctor fails to connect rather than emitting a per-node finding. Asserting
# the address proves the context actually switched clusters.
DOC=$($K doctor -t "$WORK/multi.yaml" --context elsewhere 2>&1) || true
assert_contains "--context elsewhere targets the other cluster's server" "$DOC" "10.255.255.1"
OUT=$($K check -t "$WORK/multi.yaml" --context nope 2>&1)
assert_contains "an unknown context lists the real ones" "$OUT" "have: sandbox, elsewhere"

# ── 11. doctor on a healthy cluster ──────────────────────────────────────────
step "11. doctor on a healthy cluster — zero findings"
kctl delete deployment e2e-web -n e2e-ns >/dev/null 2>&1
kctl delete namespace e2e-ns >/dev/null 2>&1
sleep 8
OUT=$($K doctor -t $TARGETS 2>&1)
assert_contains "healthy verdict" "$OUT" "no issues detected"
$K doctor -t $TARGETS >/dev/null 2>&1; assert_exit "exit 0 when healthy" 0 $?
OUT=$($K doctor -t $TARGETS --json 2>&1)
assert_contains "--json reports healthy" "$OUT" '"healthy": true'

# an unreachable node must never read as healthy
sed "s/$AGENT1_IP/10.255.255.2/" "$TARGETS" > "$WORK/ghost.yaml"
OUT=$($K doctor -t "$WORK/ghost.yaml" --json 2>&1); RC=$?
assert_contains "an unreachable node is reported" "$OUT" '"signature_id": "node.unreachable"'
assert_exit "and is not a clean bill of health" 2 $RC

# ── 12. fault: k3s-agent down ────────────────────────────────────────────────
step "12. FAULT: stop k3s-agent on agent1"
ssh "${SSHO[@]}" "sandbox@$AGENT1_IP" 'sudo systemctl stop k3s-agent' 2>/dev/null
echo "  (waiting up to 60s for the node to go NotReady)"
DETECTED=0
for _ in $(seq 1 12); do
  sleep 5
  # capture before grepping: doctor exits 2 on findings and pipefail would
  # make that beat grep's success
  DOC=$($K doctor -t $TARGETS --json 2>/dev/null) || true
  if printf '%s' "$DOC" | grep -q '"signature_id": "node.notready-k3s-down"'; then DETECTED=1; break; fi
done
[ "$DETECTED" = 1 ] && ok "doctor catches the stopped service" || bad "doctor missed the stopped k3s-agent"
$K doctor -t $TARGETS >/dev/null 2>&1; assert_exit "exit 2 when faulted" 2 $?

# ── 13. recover ──────────────────────────────────────────────────────────────
step "13. RECOVER: restart k3s-agent"
ssh "${SSHO[@]}" "sandbox@$AGENT1_IP" 'sudo systemctl start k3s-agent' 2>/dev/null
echo "  (waiting up to 90s for the node to return)"
RECOVERED=0
for _ in $(seq 1 18); do
  sleep 5
  if $K doctor -t $TARGETS >/dev/null 2>&1; then RECOVERED=1; break; fi
done
[ "$RECOVERED" = 1 ] && ok "healthy again after recovery" || bad "cluster did not recover"

# ── 14. fault sweep ──────────────────────────────────────────────────────────
if [ "$QUICK" = 0 ]; then
  step "14. Fault sweep — each fault must produce its own signature"
  if test/faults/check-all.sh imagepull crashloop pvc-pending empty-endpoints coredns oom 2>&1 | tee "$WORK/faults.log" | grep -E '^  (✓|✗)'; then :; fi
  # grep -c exits 1 on zero matches; capture the count without letting that
  # append a second "0" and break the arithmetic below
  SWEEP=$(grep -c '^  ✓' "$WORK/faults.log" 2>/dev/null); SWEEP=${SWEEP:-0}
  SWEEP_BAD=$(grep -c '^  ✗' "$WORK/faults.log" 2>/dev/null); SWEEP_BAD=${SWEEP_BAD:-0}
  PASS=$((PASS+SWEEP)); FAIL=$((FAIL+SWEEP_BAD))
  [ "$SWEEP_BAD" -gt 0 ] && FAILED_NAMES+=("fault sweep: $SWEEP_BAD undiagnosed")
else
  echo "  (--quick: skipping the fault sweep)"
fi

# ── 15. cleanup → healthy ────────────────────────────────────────────────────
step "15. Cleanup → cluster healthy again"
test/faults/fault.sh clean >/dev/null 2>&1
kctl delete deployment e2e-web --ignore-not-found >/dev/null 2>&1
sleep 12
RECOVERED=0
for _ in $(seq 1 12); do
  if $K doctor -t $TARGETS >/dev/null 2>&1; then RECOVERED=1; break; fi
  sleep 5
done
if [ "$RECOVERED" = 1 ]; then ok "healthy again"
else bad "cluster not healthy after cleanup"; $K doctor -t $TARGETS 2>&1 | sed 's/^/      /' | head -12; fi

# ── summary ───────────────────────────────────────────────────────────────────
echo
echo "════════════════════════════════════════"
echo "  E2E RESULT: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  echo
  for f in "${FAILED_NAMES[@]}"; do echo "  ✗ $f"; done
fi
echo "════════════════════════════════════════"
[ "$FAIL" -eq 0 ]
