FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go *.tmpl ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /homerealm .

FROM alpine:3.20
RUN apk add --no-cache docker-cli docker-cli-compose ca-certificates
COPY --from=build /homerealm /usr/local/bin/homerealm
EXPOSE 8090
CMD ["homerealm"]
