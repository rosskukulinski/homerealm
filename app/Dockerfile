# The build stage runs on the BUILD host's architecture and Go
# cross-compiles for the target — multi-arch images without emulating the
# compiler under QEMU, which is ~4x faster for the arm64 half.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go *.tmpl ./
ARG BUILD_COMMIT=""
ARG BUILD_BRANCH=""
ARG BUILD_PR=""
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w \
      -X main.buildCommit=${BUILD_COMMIT} \
      -X main.buildBranch=${BUILD_BRANCH} \
      -X main.buildPR=${BUILD_PR}" \
    -o /homerealm .

FROM alpine:3.24
RUN apk add --no-cache docker-cli docker-cli-compose ca-certificates
COPY --from=build /homerealm /usr/local/bin/homerealm
EXPOSE 8090
CMD ["homerealm"]
