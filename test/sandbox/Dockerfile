# Sandbox "VM" — Ubuntu with systemd + openssh-server, like a real k3s host
ARG UBUNTU_VERSION=24.04
FROM ubuntu:${UBUNTU_VERSION}

ARG NODE_HOSTNAME=sandbox-server
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    systemd systemd-sysv openssh-server sudo curl ca-certificates iproute2 \
    procps net-tools util-linux kmod iputils-ping vim \
  && rm -rf /var/lib/apt/lists/*

# Passwordless sudo for the sandbox user
RUN useradd -m -s /bin/bash -G sudo sandbox \
  && echo "sandbox:sandbox" | chpasswd \
  && echo "sandbox ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/sandbox

# SSH: allow key + password auth, start sshd via systemd
COPY sshd_config.d/*.conf /etc/sshd_config.d/
COPY ssh/authorized_keys /home/sandbox/.ssh/authorized_keys
RUN chmod 700 /home/sandbox/.ssh \
  && chmod 600 /home/sandbox/.ssh/authorized_keys \
  && chown -R sandbox:sandbox /home/sandbox/.ssh \
  && mkdir -p /run/sshd

# systemd expects to be PID 1
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
