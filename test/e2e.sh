#!/bin/bash
# =============================================================================
# k3helper E2E test — full lifecycle, run against the OrbStack sandbox
#
#   sandbox reset → SSH → check(no k3s) → bootstrap → check(green)
#   → gen → verify → deploy → doctor(healthy)
#   → fault: k3s-agent stop   → doctor detects (90%)
#   → recover                 → doctor healthy
#   → fault: OOMKill pod      → doctor detects (95%)
#   → cleanup                 → doctor healthy
#
# Usage:
#   ./test/e2e.sh            # full run incl. sandbox reset (~10 min)
#   ./test/e2e.sh --keep     # skip sandbox reset (reuses running sandbox)
#
# Exit code 0 = all assertions passed.
# =============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
TARGETS=test/sandbox/targets.sandbox.yaml
KEEP_SANDBOX=${1:-}

PASS=0; FAIL=0
step() { echo; echo "━━━ $* ━━━"; }
assert_contains() { # desc haystack needle
  if echo "$2" | grep -q "$3"; then echo "  ✓ $1"; PASS=$((PASS+1));
  else echo "  ✗ $1 — expected '$3' in:"; echo "$2" | sed 's/^/    /' | head -5; FAIL=$((FAIL+1)); fi
}
assert_matches() { # desc haystack extended-regex
  if echo "$2" | grep -qE "$3"; then echo "  ✓ $1"; PASS=$((PASS+1));
  else echo "  ✗ $1 — expected match /$3/ in:"; echo "$2" | sed 's/^/    /' | head -5; FAIL=$((FAIL+1)); fi
}
assert_exit() { # desc expected_exit actual_exit
  if [ "$2" = "$3" ]; then echo "  ✓ $1 (exit=$3)"; PASS=$((PASS+1));
  else echo "  ✗ $1 — expected exit $2, got $3"; FAIL=$((FAIL+1)); fi
}

# always rebuild: a stale bin/ would silently test a different revision
echo "building..."
go build -o bin/k3helper ./cmd/k3helper || exit 1
K=bin/k3helper

# ── 0. sandbox ────────────────────────────────────────────────────────────────
if [ "$KEEP_SANDBOX" != "--keep" ]; then
  step "0. Reset sandbox (3 fresh Ubuntu 24.04 VMs)"
  make sandbox-down >/dev/null 2>&1
  if ! test/sandbox/setup-orbstack.sh > /tmp/e2e-sandbox.log 2>&1; then
    echo "sandbox setup failed:"; tail -5 /tmp/e2e-sandbox.log; exit 1
  fi
  grep -q "sandbox ready" /tmp/e2e-sandbox.log && echo "  ✓ 3 VMs up, SSH verified"
fi
SERVER_IP=$(grep -A3 'name: server' "$TARGETS" | grep 'host:' | awk '{print $2}')
SSHOPTS="-i test/sandbox/ssh/id_ed25519 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5"
kctl() { ssh $SSHOPTS sandbox@$SERVER_IP "sudo k3s kubectl $*" 2>/dev/null; }

# ── 1. check before install: k3s missing ─────────────────────────────────────
if [ "$KEEP_SANDBOX" != "--keep" ]; then
  step "1. check BEFORE install — k3s must be reported missing"
  OUT=$($K check -t $TARGETS 2>&1)
  assert_contains "k3s service reported down" "$OUT" "k3s is inactive"
fi

# ── 2. bootstrap ──────────────────────────────────────────────────────────────
step "2. Bootstrap k3s on all 3 nodes over SSH"
OUT=$($K vm setup -t $TARGETS \
  --server-extra-args "--snapshotter=native --disable=traefik" \
  --agent-extra-args "--snapshotter=native" \
  --kubeconfig sandbox-kubeconfig.yaml 2>&1)
assert_contains "cluster ready" "$OUT" "cluster ready"

# ── 3. check after install ───────────────────────────────────────────────────
step "3. check AFTER install — all green"
sleep 10
OUT=$($K check -t $TARGETS 2>&1)
assert_contains "server k3s active" "$OUT" "k3s is active"
assert_contains "agent k3s-agent active" "$OUT" "k3s-agent is active"
# The OK branch of the disk check reads "disk N% used"; the Warn/Fail branches
# read "disk N% full". Match the verdict, not a specific percentage, so the
# assertion tracks disk pressure rather than the base image's fill level.
assert_matches "no disk pressure" "$OUT" "disk [0-9]+% used"

# ── 4. gen → verify → deploy ─────────────────────────────────────────────────
step "4. gen → verify → deploy"
$K gen deployment e2e-web -i nginx:alpine -r 1 -p 80 -o /tmp/e2e-web.yaml
OUT=$($K verify /tmp/e2e-web.yaml 2>&1)
assert_contains "generated manifest verifies (offline)" "$OUT" "Deployment/e2e-web"
# validation layer 3: the real API server must accept it too
OUT=$($K verify /tmp/e2e-web.yaml --dry-run-server -t $TARGETS 2>&1)
assert_contains "generated manifest verifies (server dry-run)" "$OUT" "Deployment/e2e-web"
# deploy ships the local manifest to the target itself — no manual scp here,
# otherwise the e2e would not be testing the path real users take.
OUT=$($K deploy -f /tmp/e2e-web.yaml -t $TARGETS 2>&1)
assert_contains "applied" "$OUT" "applied deployment.apps/e2e-web"
assert_contains "rolled out" "$OUT" "rolled out deployment.apps/e2e-web"
PODS=$(kctl get pods -l app=e2e-web --no-headers 2>/dev/null | grep -c Running)
if [ "$PODS" -ge 1 ]; then echo "  ✓ pod Running in cluster"; PASS=$((PASS+1)); else echo "  ✗ pod not running"; FAIL=$((FAIL+1)); fi

# ── 5. doctor on healthy cluster ─────────────────────────────────────────────
step "5. doctor on healthy cluster — zero findings"
sleep 5
OUT=$($K doctor -t $TARGETS 2>&1)
assert_contains "healthy verdict" "$OUT" "no issues detected"
$K doctor -t $TARGETS >/dev/null 2>&1; assert_exit "exit code 0 when healthy" 0 $?

# ── 6. fault: k3s-agent stopped ───────────────────────────────────────────────
step "6. FAULT: stop k3s-agent on agent1"
AGENT1_IP=$(grep -A3 'name: agent1' "$TARGETS" | grep 'host:' | awk '{print $2}')
ssh $SSHOPTS sandbox@$AGENT1_IP 'sudo systemctl stop k3s-agent' 2>/dev/null
echo "  (waiting 40s for node to go NotReady)"; sleep 40
OUT=$($K doctor -t $TARGETS 2>&1)
assert_contains "doctor catches k3s down" "$OUT" "k3s service is down"
$K doctor -t $TARGETS >/dev/null 2>&1; assert_exit "exit code 2 when faulted" 2 $?

# ── 7. recover ────────────────────────────────────────────────────────────────
step "7. RECOVER: restart k3s-agent"
ssh $SSHOPTS sandbox@$AGENT1_IP 'sudo systemctl start k3s-agent' 2>/dev/null
echo "  (waiting 60s for node Ready)"; sleep 60
OUT=$($K doctor -t $TARGETS 2>&1)
assert_contains "healthy again" "$OUT" "no issues detected"

# ── 8. fault: OOMKilled ───────────────────────────────────────────────────────
step "8. FAULT: OOMKilled pod (10Mi limit, 50MB allocation)"
cat > /tmp/e2e-oom.yaml <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: e2e-oom
spec:
  restartPolicy: Always
  containers:
  - name: hog
    image: busybox:latest
    resources:
      limits:
        memory: "10Mi"
    command: ["sh", "-c", "head -c 50000000 /dev/zero | tr '\\0' 'x' > /tmp/big; VAR=$(cat /tmp/big); echo VARSIZE ${#VAR}; sleep 60"]
EOF
scp -q $SSHOPTS /tmp/e2e-oom.yaml sandbox@$SERVER_IP:/tmp/e2e-oom.yaml 2>/dev/null
kctl apply -f /tmp/e2e-oom.yaml >/dev/null
# The pod restarts on a loop (restartPolicy: Always), so a single sample of
# lastState can catch a restart whose exit the kubelet reported as a generic
# "Error" rather than "OOMKilled". Poll both state and lastState until either
# reports OOMKilled instead of betting on one 45s snapshot.
echo "  (waiting up to 90s for OOMKill)"
STATE=""
for _ in $(seq 1 18); do
  sleep 5
  STATE=$(kctl get pod e2e-oom -o jsonpath='{.status.containerStatuses[0].lastState.terminated.reason}{" "}{.status.containerStatuses[0].state.terminated.reason}' 2>/dev/null)
  case "$STATE" in *OOMKilled*) break ;; esac
done
case "$STATE" in
  *OOMKilled*) echo "  ✓ pod OOMKilled in cluster"; PASS=$((PASS+1)) ;;
  *) echo "  ✗ expected OOMKilled in state/lastState, got '$STATE'"; FAIL=$((FAIL+1)) ;;
esac
OUT=$($K doctor -t $TARGETS 2>&1)
assert_contains "doctor catches OOMKill" "$OUT" "OOMKilled"

# ── 9. cleanup ────────────────────────────────────────────────────────────────
step "9. Cleanup: remove OOM pod → healthy"
kctl delete pod e2e-oom --force --grace-period=0 >/dev/null 2>&1
sleep 12
OUT=$($K doctor -t $TARGETS 2>&1)
assert_contains "healthy again" "$OUT" "no issues detected"

# ── summary ───────────────────────────────────────────────────────────────────
echo
echo "════════════════════════════════════════"
echo "  E2E RESULT: $PASS passed, $FAIL failed"
echo "════════════════════════════════════════"
[ "$FAIL" -eq 0 ]
