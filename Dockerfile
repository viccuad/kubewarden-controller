# syntax=docker/dockerfile:1

# Build the manager binary
FROM golang:1.23 AS build

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/main.go cmd/main.go
COPY api/ api/
COPY internal/ internal/

# Build
RUN CGO_ENABLED=0 GOOS=linux GO111MODULE=on go build -a -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /workspace/manager .
# Copy the Go Modules manifests - these are used by BOM generators
# and by security scanner
COPY go.mod /go.mod
COPY go.sum /go.sum
USER 65532:65532

ENTRYPOINT ["/manager"]
