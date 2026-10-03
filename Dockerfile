# syntax=docker/dockerfile:1.7
#
# kubeswift-spin controller image. Build from the repository root:
#   docker build -t <image> .

ARG GO_IMAGE=golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d
ARG BASE_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DEFAULT_RUNTIME_IMAGE=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -buildid= \
      -X github.com/kubeswift-io/kubeswift-spin/internal/version.Version=${VERSION} \
      -X github.com/kubeswift-io/kubeswift-spin/internal/version.Commit=${COMMIT} \
      -X github.com/kubeswift-io/kubeswift-spin/internal/version.DefaultRuntimeImage=${DEFAULT_RUNTIME_IMAGE}" \
    -o /out/manager ./cmd/manager

FROM ${BASE_IMAGE}
COPY --from=build /out/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
LABEL org.opencontainers.image.title="kubeswift-spin" \
      org.opencontainers.image.description="SpinKube executor that runs SpinApps in KubeSwift sandboxes" \
      org.opencontainers.image.source="https://github.com/kubeswift-io/kubeswift-spin" \
      org.opencontainers.image.licenses="Apache-2.0"
