#!/bin/bash
# =============================================================================
# The troubleshooter's exam: inject every fault in turn, assert `doctor`
# reports the signature that fault is supposed to produce, then clean up.
#
#   test/faults/check-all.sh                    every fault
#   test/faults/check-all.sh oom pvc-pending    only these
#
# Matching is on signature IDs from `doctor --json`, not on display titles,
# so rewording a finding cannot silently break the suite.
#
# Exit 0 = every fault was diagnosed.
# =============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
FAULT=test/faults/fault.sh
TARGETS=${TARGETS:-test/sandbox/targets.sandbox.yaml}
K=bin/k3helper

go build -o "$K" ./cmd/k3helper || exit 1

# Cheapest faults first; the slow node-level ones last. disk-full is last of
# all: it fills a node's root filesystem, so kubelet evicts pods and the node
# needs time to settle afterwards. Running it earlier would leave every later
# fault racing an evicting node.
ALL=(imagepull crashloop pending pvc-pending empty-endpoints coredns oom cordon k3s-down bad-kubeconfig disk-full)
FAULTS=("$@")
[ ${#FAULTS[@]} -eq 0 ] && FAULTS=("${ALL[@]}")

LAST_FAULT=${FAULTS[${#FAULTS[@]}-1]}

PASS=0; FAIL=0

# How long the cluster needs before the fault is observable.
settle_for() {
  case "$1" in
    # kubelet only re-evaluates its eviction signals on its housekeeping
    # interval, so DiskPressure appears well after the disk is actually full
    disk-full)    echo 60 ;;
    k3s-down|oom) echo 45 ;;
    # the scheduler retries FailedScheduling with backoff, so the event can
    # take longer to appear than the pod takes to go Pending
    cordon|pending) echo 40 ;;
    crashloop)    echo 30 ;;
    imagepull)    echo 25 ;;
    coredns)      echo 10 ;;
    *)            echo 12 ;;
  esac
}

# reported <signature-id> — does doctor currently report it?
#
# The output is captured before grepping rather than piped: doctor exits 2
# when it finds anything, and under `set -o pipefail` that exit code wins over
# grep's, so a piped match would report failure on every fault it caught.
reported() {
  local out
  out=$("$K" doctor -t "$TARGETS" --json 2>/dev/null) || true
  printf '%s' "$out" | grep -q "\"signature_id\": \"$1\""
}

for f in "${FAULTS[@]}"; do
  want=$("$FAULT" expect "$f")
  if [ -z "$want" ]; then
    echo "✗ $f — no expected signature registered"; FAIL=$((FAIL+1)); continue
  fi

  echo
  echo "━━━ fault: $f (expect $want) ━━━"
  if ! "$FAULT" "$f" >/dev/null; then
    echo "  ✗ injection failed"
    FAIL=$((FAIL+1)); "$FAULT" clean >/dev/null; continue
  fi

  # Poll rather than sleeping the worst case: most faults surface sooner.
  budget=$(settle_for "$f")
  found=0
  for _ in $(seq 1 $((budget / 5 + 1))); do
    sleep 5
    if reported "$want"; then found=1; break; fi
  done

  if [ "$found" = 1 ]; then
    echo "  ✓ doctor reported $want"
    PASS=$((PASS+1))
  else
    echo "  ✗ doctor did not report $want within ${budget}s. It found:"
    "$K" doctor -t "$TARGETS" 2>&1 | sed 's/^/      /' | head -12
    FAIL=$((FAIL+1))
  fi

  "$FAULT" clean >/dev/null
  sleep 5

  # Node-level faults outlive their injection: a restarted node re-registers
  # on its own schedule, and kubelet holds DiskPressure for its
  # eviction-pressure-transition-period (5 minutes by default) *after* the
  # disk is free again, so that a node on the threshold cannot flap. Waiting
  # for the condition to actually clear stops one fault's aftermath being
  # blamed on the next one — but only when there is a next one, because
  # waiting out kubelet's timer at the end of the run buys nothing.
  case "$f" in
    disk-full|k3s-down)
      if [ "$f" = "$LAST_FAULT" ]; then
        echo "  (leaving $want to clear on kubelet's own schedule; nothing follows it)"
      else
        echo "  waiting for $want to clear before the next fault..."
        for _ in $(seq 1 72); do
          reported "$want" || break
          sleep 5
        done
        if reported "$want"; then
          echo "  ! $want still reported 6 minutes after cleanup — the cluster has not recovered"
        fi
      fi
      ;;
  esac
done

echo
echo "════════════════════════════════════════"
echo "  FAULT MATRIX: $PASS passed, $FAIL failed"
echo "════════════════════════════════════════"
[ "$FAIL" -eq 0 ]
