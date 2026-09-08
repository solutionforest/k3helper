#!/bin/bash
# Creates 3 OrbStack Linux VMs as the k3helper sandbox (proper VMs, not containers:
# OrbStack Docker containers share the macOS kernel and break kubelet PLEG).
# Idempotent: safe to re-run. Verifies SSH at the end.
set -euo pipefail

# project root = parent of this script's dir
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
KEY="$ROOT/test/sandbox/ssh/authorized_keys"
PRIVKEY="$ROOT/test/sandbox/ssh/id_ed25519"

[ -f "$KEY" ] || { echo "missing $KEY — generate with: ssh-keygen -t ed25519 -f $PRIVKEY -N ''"; exit 1; }

create() {
  local name=$1
  orb create ubuntu:24.04 "$name" 2>/dev/null || echo "(machine $name exists, reconfiguring)" >&2
  orb -m "$name" sudo bash -c "apt-get update -qq && apt-get install -y -qq openssh-server sudo curl" >/dev/null
  orb -m "$name" sudo bash -c "useradd -m -s /bin/bash -G sudo sandbox 2>/dev/null; echo 'sandbox:sandbox' | chpasswd; echo 'sandbox ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/sandbox; systemctl enable --now ssh"
  orb -m "$name" sudo bash -c "cat > /home/sandbox/.ssh/authorized_keys" < "$KEY"
  orb -m "$name" sudo bash -c "chmod 700 /home/sandbox/.ssh; chmod 600 /home/sandbox/.ssh/authorized_keys; chown -R sandbox:sandbox /home/sandbox/.ssh"
  orb -m "$name" hostname -I | awk '{print $1}'
}

IP_SERVER=$(create sandbox-server)
IP_AGENT1=$(create sandbox-agent1)
IP_AGENT2=$(create sandbox-agent2)

# regenerate targets with live IPs
TARGETS="$ROOT/test/sandbox/targets.sandbox.yaml"
cat > "$TARGETS" <<EOF
cluster: sandbox
nodes:
  - name: server
    role: server
    host: $IP_SERVER
    port: 22
    user: sandbox
    key: test/sandbox/ssh/id_ed25519
  - name: agent1
    role: agent
    host: $IP_AGENT1
    port: 22
    user: sandbox
    key: test/sandbox/ssh/id_ed25519
  - name: agent2
    role: agent
    host: $IP_AGENT2
    port: 22
    user: sandbox
    key: test/sandbox/ssh/id_ed25519
EOF
echo "targets written: $TARGETS"

# verify SSH for real (fail the script if unreachable)
ok=0
for ip in "$IP_SERVER" "$IP_AGENT1" "$IP_AGENT2"; do
  if ssh -i "$PRIVKEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
     -o ConnectTimeout=5 -o BatchMode=yes sandbox@"$ip" 'echo ok' 2>/dev/null | grep -q ok; then
    echo "✓ ssh $ip"
    ok=$((ok+1))
  else
    echo "✗ ssh $ip FAILED"
  fi
done
[ "$ok" -eq 3 ] || { echo "SSH verification failed ($ok/3)"; exit 1; }
echo "sandbox ready: server=$IP_SERVER agent1=$IP_AGENT1 agent2=$IP_AGENT2"
