# SPDX-License-Identifier: APACHE-2.0

# STEP 1: Build kindnetd binary
FROM --platform=$BUILDPLATFORM golang:1.23 AS builder
ARG TARGETARCH BUILDPLATFORM
# copy in sources
WORKDIR /src
COPY . .
# build
RUN CGO_ENABLED=0 GOARCH=$TARGETARCH go build -o /go/bin/kindnetd ./cmd/kindnetd
WORKDIR /src/cmd/cni-kindnet
# sqlite requires CGO
RUN if [ "$TARGETARCH" = "arm64" ] ; then \
      apt-get update && apt-get -y install gcc-aarch64-linux-gnu g++-aarch64-linux-gnu ;\
      CC=aarch64-linux-gnu-gcc CGO_ENABLED=1 GOARCH=$TARGETARCH go build -o /go/bin/cni-kindnet . ;\
  else \
      CGO_ENABLED=1 GOARCH=$TARGETARCH go build -o /go/bin/cni-kindnet . ;\
  fi

# STEP 2: Build small image
FROM --platform=${BUILDPLATFORM} registry.k8s.io/build-image/distroless-iptables:v0.6.6
COPY --from=builder --chown=root:root /go/bin/kindnetd /bin/kindnetd
COPY --from=builder --chown=root:root /go/bin/cni-kindnet /opt/cni/bin/cni-kindnet
CMD ["/bin/kindnetd"]
