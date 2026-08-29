FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go *.tmpl ./
ARG BUILD_COMMIT=""
ARG BUILD_BRANCH=""
ARG BUILD_PR=""
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w \
      -X main.buildCommit=${BUILD_COMMIT} \
      -X main.buildBranch=${BUILD_BRANCH} \
      -X main.buildPR=${BUILD_PR}" \
    -o /homerealm .

FROM alpine:3.20
RUN apk add --no-cache docker-cli docker-cli-compose ca-certificates
COPY --from=build /homerealm /usr/local/bin/homerealm
EXPOSE 8090
CMD ["homerealm"]
