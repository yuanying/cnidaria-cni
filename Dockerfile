# The node daemon image. Built for linux/amd64 and linux/arm64 with docker buildx.
#
# The image also carries the reference CNI plugins (bridge, host-local, portmap):
# cnidaria has no CNI binary of its own (ADR 0001), and the DaemonSet's init container
# copies these into /opt/cni/bin on the node.
ARG CNI_PLUGINS_VERSION=v1.9.1

FROM --platform=$BUILDPLATFORM golang:1.27-trixie AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /out/cnidaria ./cmd/cnidaria

# Fetching the plugins runs nothing of the target architecture, only curl and tar, so
# this stage stays on the build platform and the tarball is chosen by TARGETARCH.
FROM --platform=$BUILDPLATFORM debian:trixie-slim AS plugins
ARG TARGETARCH
ARG CNI_PLUGINS_VERSION
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
# The tarball is checked against the .sha256 file published next to it upstream.
RUN base="https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS_VERSION}" \
    && tgz="cni-plugins-linux-${TARGETARCH}-${CNI_PLUGINS_VERSION}.tgz" \
    && curl -fsSLO "${base}/${tgz}" \
    && curl -fsSLO "${base}/${tgz}.sha256" \
    && sha256sum -c "${tgz}.sha256" \
    && mkdir -p /opt/cni/bin \
    && tar -xz -C /opt/cni/bin -f "${tgz}" ./bridge ./host-local ./portmap \
    && rm -f "${tgz}" "${tgz}.sha256"

# iptables carries both the nft and the legacy backend: the forward chain goes through
# whichever of the two kube-proxy uses on the node (ADR 0003).
FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends iproute2 iptables nftables \
    && rm -rf /var/lib/apt/lists/*
COPY --from=plugins /opt/cni/bin /opt/cni/bin
COPY --from=build /out/cnidaria /usr/local/bin/cnidaria
ENTRYPOINT ["/usr/local/bin/cnidaria"]
