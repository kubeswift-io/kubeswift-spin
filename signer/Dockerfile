# syntax=docker/dockerfile:1.7
#
# kubeswift-spin cache signer image (ADR 0014): the signer and cosign, a
# cosign version whose signatures KubeSwift's cosign verifies (checked with
# the compiled-cache e2e before cosign moves). Build from the
# repository root:
#   docker build -f signer/Dockerfile -t <image> .

ARG GO_IMAGE=golang:1.27.3-bookworm@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5
ARG BASE_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api api
COPY cmd cmd
COPY internal internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -buildid= \
      -X github.com/kubeswift-io/kubeswift-spin/internal/version.Version=${VERSION} \
      -X github.com/kubeswift-io/kubeswift-spin/internal/version.Commit=${COMMIT}" \
    -o /out/cache-signer ./cmd/cache-signer

# cosign v3.1.4 built from source with this repository's Go toolchain and
# the dependency versions pinned in signer/cosign (go.mod, go.sum), so its
# dependencies are scanned and updated with the rest of the project. The
# release binaries embed an older Go and dependencies with fixed CVEs.
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS cosign
ARG TARGETOS
ARG TARGETARCH
ARG COSIGN_VERSION=v3.1.4
WORKDIR /cosign
COPY signer/cosign/go.mod signer/cosign/go.sum signer/cosign/tools.go ./
RUN go mod download
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -buildid= -X sigs.k8s.io/release-utils/version.gitVersion=${COSIGN_VERSION}" \
    -o /out/cosign github.com/sigstore/cosign/v3/cmd/cosign

FROM ${BASE_IMAGE}
ARG COSIGN_VERSION=v3.1.4
COPY --from=build /out/cache-signer /cache-signer
COPY --from=cosign /out/cosign /usr/local/bin/cosign
COPY signer/signing-config.json /etc/kubeswift-spin-signer/signing-config.json
USER 65532:65532
ENTRYPOINT ["/cache-signer"]
LABEL org.opencontainers.image.title="kubeswift-spin-cache-signer" \
      org.opencontainers.image.description="Signs kubeswift-spin compiled Wasmtime caches" \
      org.opencontainers.image.source="https://github.com/kubeswift-io/kubeswift-spin" \
      org.opencontainers.image.licenses="Apache-2.0" \
      io.kubeswift.spin.cosign.version="${COSIGN_VERSION}"
