# syntax=docker/dockerfile:1.7
#
# kubeswift-spin runtime rootfs: the OCI image every kubeswift-spin
# SwiftSandbox boots. It contains the pinned static Spin binary, the
# entrypoint that drops root before exec'ing Spin, and the CA bundle and
# passwd entries from distroless/static. There is no shell and no package
# manager. See docs/runtime-image.md.
#
# Build from the repository root:
#   docker build -f runtime/Dockerfile -t <image> .

ARG GO_IMAGE=golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d
ARG BASE_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS entrypoint
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/spin-entrypoint cmd/spin-entrypoint
COPY internal/runtimecontract internal/runtimecontract
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -buildid=" -o /out/kubeswift-spin-entrypoint ./cmd/spin-entrypoint \
 && mkdir -p /out/var/lib/kubeswift-spin

# Spin is downloaded from the GitHub release and verified against the
# SHA-256 published in checksums-<version>.txt for that release. The digests
# are pinned here, so a replaced release asset fails the build.
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS spin
ARG TARGETARCH
ARG SPIN_VERSION=v4.2.1
ARG SPIN_SHA256_AMD64=7ba4da8dd39f35a3bcd461e75b29433d292eb997b0c10de6c53924fb73475ab9
ARG SPIN_SHA256_ARM64=a1c0c3ac2227c0dc91a08c0696318081de6279e96bad397f050ab6459c85ee6f
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) arch=amd64; sum="$SPIN_SHA256_AMD64" ;; \
      arm64) arch=aarch64; sum="$SPIN_SHA256_ARM64" ;; \
      *) echo "unsupported architecture $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSL --proto '=https' --tlsv1.2 -o /tmp/spin.tar.gz \
      "https://github.com/spinframework/spin/releases/download/${SPIN_VERSION}/spin-${SPIN_VERSION}-static-linux-${arch}.tar.gz"; \
    echo "${sum}  /tmp/spin.tar.gz" | sha256sum -c -; \
    mkdir -p /out; \
    tar -xzf /tmp/spin.tar.gz -C /out spin LICENSE; \
    chmod 0755 /out/spin

FROM ${BASE_IMAGE}
ARG SPIN_VERSION=v4.2.1
COPY --from=spin /out/spin /usr/local/bin/spin
COPY --from=spin /out/LICENSE /usr/share/doc/spin/LICENSE
COPY --from=entrypoint /out/kubeswift-spin-entrypoint /usr/local/bin/kubeswift-spin-entrypoint
COPY --from=entrypoint --chown=65532:65532 /out/var/lib/kubeswift-spin /var/lib/kubeswift-spin
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/kubeswift-spin-entrypoint"]
CMD ["--version"]
LABEL org.opencontainers.image.title="kubeswift-spin-runtime" \
      org.opencontainers.image.description="Spin runtime rootfs for KubeSwift sandboxes" \
      org.opencontainers.image.source="https://github.com/kubeswift-io/kubeswift-spin" \
      org.opencontainers.image.licenses="AGPL-3.0-only AND Apache-2.0 WITH LLVM-exception" \
      io.kubeswift.spin.version="${SPIN_VERSION}"
