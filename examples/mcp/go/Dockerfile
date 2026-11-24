FROM golang:1.25.5-bookworm@sha256:d9132cce84391efab786495288756d60e1da215b1f94e87860aeefc3d4c45b6d
WORKDIR /deps
COPY go.mod go.sum ./
RUN go mod download all && go mod verify && chmod -R a+rX /go/pkg/mod
RUN useradd --create-home --uid 1000 runner
ENV HOME=/home/runner GOCACHE=/home/runner/.cache/go-build GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off
USER runner
WORKDIR /home/runner
