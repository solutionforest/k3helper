#!/bin/bash
# =============================================================================
# Fault injection for the k3helper sandbox.
#
#   test/faults/fault.sh <name>     inject a fault
#   test/faults/fault.sh clean      undo every fault
#   test/faults/fault.sh list       list the faults and what should be detected
#
# Each fault is paired with the doctor signature it is expected to trigger, so
# `make fault-check-all` can assert the troubleshooter actually catches them.
# =============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
TARGETS=${TARGETS:-test/sandbox/targets.sandbox.yaml}
KEY=test/sandbox/ssh/id_ed25519
SSH_OPTS=(-i "$KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8 -o BatchMode=yes)

[ -f "$TARGETS" ] || { echo "no targets file at $TARGETS — run 'make sandbox-up'"; exit 1; }

node_host() { # node_host <name>
  grep -A4 "name: $1\$" "$TARGETS" | grep 'host:' | head -1 | awk '{print $2}'
}
SERVER=$(node_host server)
AGENT1=$(node_host agent1)

on() { # on <host> <command...>
  ssh "${SSH_OPTS[@]}" "sandbox@$1" "$2" 2>/dev/null
}
kctl() { on "$SERVER" "sudo k3s kubectl $1"; }

# fault name → doctor signature that must fire.
# A case statement rather than an associative array: macOS ships bash 3.2,
# which has no `declare -A`.
expect_for() {
  case "$1" in
    k3s-down)        echo "node.notready-k3s-down" ;;
    disk-full)       echo "node.diskpressure" ;;
    imagepull)       echo "pod.imagepull" ;;
    crashloop)       echo "pod.crashloop" ;;
    oom)             echo "pod.oom" ;;
    pending)         echo "pod.pending-sched" ;;
    pvc-pending)     echo "storage.pvc-pending" ;;
    cordon)          echo "pod.pending-sched" ;;
    coredns)         echo "network.coredns" ;;
    empty-endpoints) echo "network.empty-endpoints" ;;
    bad-kubeconfig)  echo "cluster.kubeconfig" ;;
    *)               echo "" ;;
  esac
}

FAULT_NAMES="bad-kubeconfig cordon coredns crashloop disk-full empty-endpoints imagepull k3s-down oom pending pvc-pending"

usage() {
  echo "usage: $0 <fault|clean|list|expect>"
  echo
  printf '  %-18s %s\n' "FAULT" "EXPECTED SIGNATURE"
  for f in $FAULT_NAMES; do
    printf '  %-18s %s\n' "$f" "$(expect_for "$f")"
  done
}

apply_manifest() { # apply_manifest <name> <<<yaml
  local name=$1
  cat > "/tmp/fault-$name.yaml"
  scp -q "${SSH_OPTS[@]}" "/tmp/fault-$name.yaml" "sandbox@$SERVER:/tmp/fault-$name.yaml"
  kctl "apply -f /tmp/fault-$name.yaml" >/dev/null
}

case "${1:-}" in

  list) usage; exit 0 ;;

  # print the signature a fault should trigger (used by fault-check-all)
  expect) expect_for "${2:?fault name required}"; exit 0 ;;

  k3s-down)
    echo "stopping k3s-agent on agent1 ($AGENT1)"
    on "$AGENT1" 'sudo systemctl stop k3s-agent'
    echo "  node goes NotReady after ~40s"
    ;;

  disk-full)
    echo "filling the root filesystem on agent1 ($AGENT1)"
    # Leave a little headroom so the node stays reachable for diagnosis.
    on "$AGENT1" 'sudo fallocate -l $(($(df --output=avail -k / | tail -1) * 1024 - 400000000)) /bigfile 2>/dev/null || sudo dd if=/dev/zero of=/bigfile bs=1M count=2000'
    on "$AGENT1" 'df -h / | tail -1'
    ;;

  imagepull)
    echo "deploying a pod with an image that does not exist"
    apply_manifest imagepull <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: fault-imagepull
spec:
  containers:
    - name: c
      image: example.invalid/nope:doesnotexist
EOF
    ;;

  crashloop)
    echo "deploying a pod whose command exits non-zero immediately"
    apply_manifest crashloop <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: fault-crashloop
spec:
  restartPolicy: Always
  containers:
    - name: c
      image: busybox:latest
      command: ["sh", "-c", "echo starting; exit 1"]
EOF
    ;;

  oom)
    echo "deploying a pod that allocates far past its memory limit"
    apply_manifest oom <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: fault-oom
spec:
  restartPolicy: Always
  containers:
    - name: hog
      image: busybox:latest
      resources:
        limits:
          memory: "10Mi"
      command: ["sh", "-c", "head -c 50000000 /dev/zero | tr '\\0' 'x' > /tmp/big; VAR=$(cat /tmp/big); sleep 60"]
EOF
    ;;

  pending)
    echo "deploying a pod requesting more CPU than the cluster has"
    apply_manifest pending <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: fault-pending
spec:
  containers:
    - name: c
      image: busybox:latest
      command: ["sleep", "600"]
      resources:
        requests:
          cpu: "100"
EOF
    ;;

  pvc-pending)
    echo "creating a PVC bound to a storage class that does not exist"
    apply_manifest pvc <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: fault-pvc
spec:
  storageClassName: does-not-exist
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
EOF
    ;;

  cordon)
    # Every node, not just the agents: leaving the server schedulable lets the
    # pod land there and the fault never materialises.
    echo "cordoning every node, then scheduling a pod"
    for n in $(kctl 'get nodes -o name'); do kctl "cordon ${n#node/}" >/dev/null; done
    apply_manifest cordon <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: fault-cordon
spec:
  containers:
    - name: c
      image: busybox:latest
      command: ["sleep", "600"]
  nodeSelector:
    kubernetes.io/os: linux
  tolerations: []
EOF
    ;;

  coredns)
    echo "scaling CoreDNS to zero replicas"
    kctl 'scale deployment coredns -n kube-system --replicas=0' >/dev/null
    ;;

  empty-endpoints)
    echo "creating a Service whose selector matches no pod"
    apply_manifest endpoints <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: fault-orphan
spec:
  selector:
    app: nothing-matches-this
  ports:
    - port: 80
EOF
    ;;

  bad-kubeconfig)
    echo "corrupting the kubeconfig on the server ($SERVER)"
    on "$SERVER" 'sudo cp /etc/rancher/k3s/k3s.yaml /etc/rancher/k3s/k3s.yaml.bak && echo "not: [valid" | sudo tee /etc/rancher/k3s/k3s.yaml >/dev/null'
    ;;

  clean)
    echo "removing injected faults..."
    on "$SERVER" 'sudo test -f /etc/rancher/k3s/k3s.yaml.bak && sudo mv /etc/rancher/k3s/k3s.yaml.bak /etc/rancher/k3s/k3s.yaml'
    for host in $(grep 'host:' "$TARGETS" | awk '{print $2}'); do
      on "$host" 'sudo rm -f /bigfile; sudo systemctl start k3s 2>/dev/null; sudo systemctl start k3s-agent 2>/dev/null'
    done
    kctl 'delete pod fault-imagepull fault-crashloop fault-oom fault-pending fault-cordon --ignore-not-found --force --grace-period=0' >/dev/null
    # PVCs are removed after their consumers, and confirmed: a Pending PVC
    # left behind fires storage.pvc-pending in every later diagnosis.
    kctl 'delete pvc fault-pvc --ignore-not-found --timeout=30s' >/dev/null
    for _ in 1 2 3 4 5; do
      kctl 'get pvc fault-pvc' 2>/dev/null | grep -q fault-pvc || break
      sleep 2
    done
    kctl 'delete svc fault-orphan --ignore-not-found' >/dev/null
    kctl 'scale deployment coredns -n kube-system --replicas=1' >/dev/null
    for n in $(kctl 'get nodes -o name'); do kctl "uncordon ${n#node/}" >/dev/null; done
    rm -f /tmp/fault-*.yaml
    echo "faults cleaned"
    ;;

  ""|-h|--help) usage; exit 0 ;;

  *)
    echo "unknown fault: $1"; echo; usage; exit 1 ;;
esac
