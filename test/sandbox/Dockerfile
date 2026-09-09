# Sandbox "VM" — Ubuntu with systemd + openssh-server, like a real k3s host
ARG UBUNTU_VERSION=24.04
FROM ubuntu:${UBUNTU_VERSION}

ARG NODE_HOSTNAME=sandbox-server
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    systemd systemd-sysv openssh-server sudo curl ca-certificates iproute2 \
    procps net-tools util-linux kmod iputils-ping vim \
    # k3s brings its own iptables, but the host tools are what a real node has,
    # and ethtool is needed to turn off the veth checksum offload that breaks
    # flannel traffic between containers on some kernels.
    iptables ethtool conntrack socat \
  && rm -rf /var/lib/apt/lists/*

# Passwordless sudo for the sandbox user
RUN useradd -m -s /bin/bash -G sudo sandbox \
  && echo "sandbox:sandbox" | chpasswd \
  && echo "sandbox ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/sandbox

# SSH: allow key + password auth, start sshd via systemd
COPY sshd_config.d/*.conf /etc/ssh/sshd_config.d/
COPY ssh/authorized_keys /home/sandbox/.ssh/authorized_keys
RUN chmod 700 /home/sandbox/.ssh \
  && chmod 600 /home/sandbox/.ssh/authorized_keys \
  && chown -R sandbox:sandbox /home/sandbox/.ssh \
  && mkdir -p /run/sshd

# Everything a kubelet expects from a machine that a container does not
# provide by itself. Run as a unit rather than an entrypoint script because
# systemd is PID 1 here, and ordering it before k3s is what guarantees the
# kubelet never starts against a half-prepared host.
COPY node-prep.sh /usr/local/sbin/node-prep.sh
RUN chmod +x /usr/local/sbin/node-prep.sh
RUN printf '%s\n' \
  '[Unit]' \
  'Description=Prepare a container to host a kubelet' \
  'DefaultDependencies=no' \
  'After=sysinit.target' \
  'Before=multi-user.target k3s.service k3s-agent.service' \
  '' \
  '[Service]' \
  'Type=oneshot' \
  'RemainAfterExit=yes' \
  'ExecStart=/usr/local/sbin/node-prep.sh' \
  '' \
  '[Install]' \
  'WantedBy=multi-user.target' \
  > /etc/systemd/system/node-prep.service

RUN systemctl enable ssh node-prep

# systemd expects to be PID 1
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
