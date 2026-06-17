# Build: static Go binary
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG RELUME_VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${RELUME_VERSION}" -o /relume ./cmd/relume

# Runtime: slim image with CA certs (for cloud discovery via HTTPS)
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /relume /usr/local/bin/relume
VOLUME /data
WORKDIR /data
# Default: serve. Invoke setup/link/discover via `docker compose run`.
# WORKDIR=/data, so the default config (relume.json) already lands in the volume.
ENTRYPOINT ["relume"]
CMD ["serve"]
