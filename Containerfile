# Build: static Go binary
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG RELUMETV_VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${RELUMETV_VERSION}" -o /relumetv ./cmd/relumetv

# Runtime: slim image with CA certs (for cloud discovery via HTTPS)
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /relumetv /usr/local/bin/relumetv
VOLUME /data
WORKDIR /data
# Default: serve. Invoke setup/link/discover via `docker compose run`.
# WORKDIR=/data, so the default config (relumetv.json) already lands in the volume.
ENTRYPOINT ["relumetv"]
CMD ["serve"]
