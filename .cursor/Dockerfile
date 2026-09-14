ARG GO_VERSION=1.26.5
ARG GOLANGCI_LINT_VERSION=v2.11.4

FROM golang:${GO_VERSION}-bookworm AS go-toolchain
FROM golangci/golangci-lint:${GOLANGCI_LINT_VERSION} AS lint-toolchain

FROM ubuntu:24.04

ARG DEBIAN_FRONTEND=noninteractive
ARG DOCKER_ENGINE_VERSION=29.8.0

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      build-essential \
      ca-certificates \
      curl \
      fuse-overlayfs \
      git \
      gnupg \
      iptables \
      jq \
      openssh-server \
      ripgrep \
      sudo && \
    rm -rf /var/lib/apt/lists/*

RUN install -m 0755 -d /etc/apt/keyrings && \
    curl --retry 3 --retry-delay 5 -fsSL https://download.docker.com/linux/ubuntu/gpg \
      | gpg --dearmor -o /etc/apt/keyrings/docker.gpg && \
    chmod a+r /etc/apt/keyrings/docker.gpg && \
    source /etc/os-release && \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
      > /etc/apt/sources.list.d/docker.list && \
    docker_package_version="5:${DOCKER_ENGINE_VERSION}-1~ubuntu.${VERSION_ID}~${VERSION_CODENAME}" && \
    apt-get update && \
    apt-get install -y --no-install-recommends --allow-downgrades \
      -o Dpkg::Options::=--force-confdef \
      -o Dpkg::Options::=--force-confold \
      "docker-ce=${docker_package_version}" \
      "docker-ce-cli=${docker_package_version}" \
      containerd.io \
      docker-buildx-plugin \
      docker-compose-plugin && \
    rm -rf /var/lib/apt/lists/*

RUN install -d /etc/docker && \
    printf '%s\n' \
      '{' \
      '  "storage-driver": "fuse-overlayfs",' \
      '  "features": { "containerd-snapshotter": false }' \
      '}' > /etc/docker/daemon.json && \
    update-alternatives --set iptables /usr/sbin/iptables-legacy && \
    update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy

RUN if ! id -u ubuntu >/dev/null 2>&1; then useradd -m -s /bin/bash ubuntu; fi && \
    groupadd -f docker && \
    usermod -aG docker ubuntu && \
    usermod -aG sudo ubuntu && \
    printf '%s\n' 'ubuntu ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/ubuntu && \
    chmod 0440 /etc/sudoers.d/ubuntu && \
    install -d -o ubuntu -g ubuntu /home/ubuntu/go /home/ubuntu/.cache && \
    install -d /etc/ssh/sshd_config.d && \
    printf '%s\n' \
      'PasswordAuthentication no' \
      'ChallengeResponseAuthentication no' \
      'UsePAM no' > /etc/ssh/sshd_config.d/disable_password_auth.conf

COPY --from=go-toolchain /usr/local/go /usr/local/go
COPY --from=lint-toolchain /usr/bin/golangci-lint /usr/local/bin/golangci-lint

# Cloud Agent install/start do not inherit image ENV PATH, so expose the
# toolchain on the default PATH used by bash .cursor/install.sh.
RUN ln -sf /usr/local/go/bin/go /usr/local/bin/go && \
    ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt

ENV HOME=/home/ubuntu
ENV GOPATH=/home/ubuntu/go
ENV GOTOOLCHAIN=local
ENV PATH=/usr/local/go/bin:/home/ubuntu/go/bin:$PATH
