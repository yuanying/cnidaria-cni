# The runtime environment for the netns integration tests.
#
# The tests must run on a machine that has no nft and on which nothing is to be
# installed, so the tools live only inside this image. It is started as a sibling
# container with --privileged and the network namespaces are built inside it
# (ADR 0008).
#
# The image also carries the reference CNI plugins: the testbed attaches pods to a
# node's bridge through the real bridge / host-local / portmap plugins driven by the
# real conflist (ADR 0001), not through a stand-in.
ARG CNI_PLUGINS_VERSION=v1.9.1

FROM golang:1.27-trixie
ARG CNI_PLUGINS_VERSION

# kmod is for modprobe: br_netfilter (ADR 0002) is loaded from inside the privileged
# container when the host does not have it yet, which is why /lib/modules is mounted.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        conntrack \
        curl \
        iproute2 \
        iptables \
        iputils-ping \
        kmod \
        nftables \
        procps \
        socat \
        tcpdump \
    && rm -rf /var/lib/apt/lists/*

# The tarball is checked against the .sha256 file published next to it upstream.
RUN arch="$(dpkg --print-architecture)" \
    && base="https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS_VERSION}" \
    && tgz="cni-plugins-linux-${arch}-${CNI_PLUGINS_VERSION}.tgz" \
    && curl -fsSLO "${base}/${tgz}" \
    && curl -fsSLO "${base}/${tgz}.sha256" \
    && sha256sum -c "${tgz}.sha256" \
    && mkdir -p /opt/cni/bin \
    && tar -xz -C /opt/cni/bin -f "${tgz}" ./bridge ./host-local ./portmap \
    && rm -f "${tgz}" "${tgz}.sha256"

# If a prerequisite is missing, fail instead of silently skipping.
ENV CNIDARIA_NETNS_REQUIRE=1

WORKDIR /workspace
