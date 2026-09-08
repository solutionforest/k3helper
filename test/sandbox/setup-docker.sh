#!/bin/bash
# =============================================================================
# Docker sandbox: three privileged systemd containers that behave like
# SSH-reachable Linux hosts, for CI runners that cannot create VMs.
#
# This is the Linux counterpart of setup-orbstack.sh and produces the same
# targets.sandbox.yaml, so every test script works against either driver.
#
# On macOS, prefer setup-orbstack.sh: Docker Desktop containers share the
# macOS kernel and kubelet's PLEG kills pods spuriously under it. On a Linux
# host the kernel is shared with a *Linux* kernel, which is what k3s expects,
# so the problem does not arise.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT/test/sandbox"
TARGETS="$ROOT/test/sandbox/targets.sandbox.yaml"
KEY="$ROOT/test/sandbox/ssh/id_ed25519"
PUB="$ROOT/test/sandbox/ssh/authorized_keys"

command -v docker >/dev/null || { echo "docker is not installed"; exit 1; }

# CI mints a throwaway key; a developer may already have one.
if [ ! -f "$PUB" ]; then
  echo "generating a sandbox SSH key..."
  mkdir -p "$(dirname "$KEY")"
  ssh-keygen -q -t ed25519 -N '' -f "$KEY"
  cp "$KEY.pub" "$PUB"
fi
chmod 600 "$KEY"

echo "building the sandbox image..."
docker compose build --quiet

echo "starting three sandbox hosts..."
docker compose up -d --force-recreate

container_ip() {
  docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$1"
}

try_ssh() { # try_ssh <ip>
  ssh -i "$KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -o ConnectTimeout=5 -o BatchMode=yes "sandbox@$1" 'echo ok' 2>/dev/null | grep -q ok
}

# Wait on a real SSH connection, not on `systemctl is-active ssh`: systemd
# reports the unit active before sshd has finished binding, which makes the
# first connection fail intermittently.
echo "waiting for sshd..."
for name in sandbox-server sandbox-agent1 sandbox-agent2; do
  ip=$(container_ip "$name")
  for i in $(seq 1 60); do
    if [ -n "$ip" ] && try_ssh "$ip"; then break; fi
    if [ "$i" = 60 ]; then
      echo "✗ sshd never became reachable in $name ($ip)"
      docker logs --tail 30 "$name"
      docker exec "$name" systemctl status ssh --no-pager 2>&1 | head -20 || true
      exit 1
    fi
    sleep 1
    ip=$(container_ip "$name")
  done
done
IP_SERVER=$(container_ip sandbox-server)
IP_AGENT1=$(container_ip sandbox-agent1)
IP_AGENT2=$(container_ip sandbox-agent2)
for v in "$IP_SERVER" "$IP_AGENT1" "$IP_AGENT2"; do
  [ -n "$v" ] || { echo "✗ could not determine a container IP"; exit 1; }
done

# Same shape as the OrbStack driver writes, so the test scripts do not care
# which one provisioned the hosts.
cat > "$TARGETS" <<EOF
cluster: sandbox
nodes:
  - name: server
    role: server
    host: $IP_SERVER
    port: 22
    user: sandbox
    key: test/sandbox/ssh/id_ed25519
    insecure_host_key: true
  - name: agent1
    role: agent
    host: $IP_AGENT1
    port: 22
    user: sandbox
    key: test/sandbox/ssh/id_ed25519
    insecure_host_key: true
  - name: agent2
    role: agent
    host: $IP_AGENT2
    port: 22
    user: sandbox
    key: test/sandbox/ssh/id_ed25519
    insecure_host_key: true
EOF
echo "targets written: $TARGETS"

ok=0
for ip in "$IP_SERVER" "$IP_AGENT1" "$IP_AGENT2"; do
  if try_ssh "$ip"; then echo "✓ ssh $ip"; ok=$((ok+1)); else echo "✗ ssh $ip FAILED"; fi
done
[ "$ok" -eq 3 ] || { echo "SSH verification failed ($ok/3)"; exit 1; }
echo "sandbox ready: server=$IP_SERVER agent1=$IP_AGENT1 agent2=$IP_AGENT2"
