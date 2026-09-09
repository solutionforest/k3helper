#!/bin/bash
# =============================================================================
# Make a container look enough like a machine for a kubelet to run on it.
#
# Runs once at boot, before k3s. Everything here is something a real VM gets
# from its own kernel and init, and a container does not — the list is what
# k3d's node image does for the same reason.
# =============================================================================
set -u

log() { echo "node-prep: $*"; }

# 1. /dev/kmsg. The kubelet opens it to read kernel messages and refuses to
#    start without it. Containers get no kmsg device, so point it at the
#    console, which is where a container's kernel messages go anyway.
if [ ! -e /dev/kmsg ]; then
  ln -sf /dev/console /dev/kmsg
  log "linked /dev/kmsg -> /dev/console"
fi

# 2. Shared mount propagation. The kubelet mounts volumes into pods and
#    requires that those mounts propagate; a private root silently breaks
#    every volume mount.
mount --make-rshared / 2>/dev/null && log "made / rshared"

# 3. Kernel modules for the CNI. They may already be loaded by the host — the
#    container shares its kernel — in which case modprobe is a no-op.
for mod in br_netfilter overlay vxlan iptable_nat ip_tables; do
  modprobe "$mod" 2>/dev/null || true
done

# 4. Bridged traffic must traverse iptables, or a Service's ClusterIP is never
#    DNAT'd for a pod talking to itself through it.
sysctl -q -w net.bridge.bridge-nf-call-iptables=1 2>/dev/null || true
sysctl -q -w net.bridge.bridge-nf-call-ip6tables=1 2>/dev/null || true
sysctl -q -w net.ipv4.ip_forward=1 2>/dev/null || true

# 5. Checksum offload on veth. A VXLAN packet leaving a veth pair can carry a
#    checksum the receiver rejects, which shows up as pods that can be pinged
#    on their own node and are unreachable from every other one. The flannel
#    interface does not exist yet at boot, so this runs as a unit that waits
#    for it rather than inline here.
cat > /usr/local/sbin/flannel-offload.sh <<'EOS'
#!/bin/bash
# Turn off tx checksum offload on the flannel interface once it appears.
for _ in $(seq 1 120); do
  if ip link show flannel.1 >/dev/null 2>&1; then
    ethtool -K flannel.1 tx-checksum-ip-generic off 2>/dev/null
    exit 0
  fi
  # host-gw and other backends never create flannel.1; nothing to do.
  sleep 2
done
EOS
chmod +x /usr/local/sbin/flannel-offload.sh
nohup /usr/local/sbin/flannel-offload.sh >/dev/null 2>&1 &

log "ready"
exit 0
