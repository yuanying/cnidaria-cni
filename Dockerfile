# The node daemon image. Built for linux/amd64 and linux/arm64 with docker buildx.
#
# The image carries no CNI plugin: the reference plugins the conflist names are the
# node's, installed with the container runtime (ADR 0001).

FROM --platform=$BUILDPLATFORM golang:1.27-trixie AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /out/cnidaria ./cmd/cnidaria

# iptables carries both the nft and the legacy backend: the forward chain goes through
# whichever of the two kube-proxy uses on the node (ADR 0003).
FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends iproute2 iptables nftables \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/cnidaria /usr/local/bin/cnidaria
ENTRYPOINT ["/usr/local/bin/cnidaria"]
