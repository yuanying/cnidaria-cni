# The runtime environment for the netns integration tests.
#
# The tests must run on a machine that has no nft and on which nothing is to be
# installed, so the tools live only inside this image. It is started as a sibling
# container with --privileged and the network namespaces are built inside it
# (ADR 0008).
FROM golang:1.27-trixie

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        conntrack \
        iproute2 \
        iputils-ping \
        nftables \
        procps \
        socat \
        tcpdump \
    && rm -rf /var/lib/apt/lists/*

# If a prerequisite is missing, fail instead of silently skipping.
ENV CNIDARIA_NETNS_REQUIRE=1

WORKDIR /workspace
