# syntax=docker/dockerfile:1.7
#
# startupbench image, for hack/perf-startup.sh. Build from the repository
# root:
#   docker build -f test/perf/startupbench/Dockerfile -t <image> .

ARG GO_IMAGE=golang:1.26.9-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c
ARG BASE_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY test/perf/startupbench test/perf/startupbench
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -buildid=" \
    -o /out/startupbench ./test/perf/startupbench

FROM ${BASE_IMAGE}
COPY --from=build /out/startupbench /startupbench
USER 65532:65532
ENTRYPOINT ["/startupbench"]
