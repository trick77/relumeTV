# Build: statische Go-Binary
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG RELUME_VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${RELUME_VERSION}" -o /relume ./cmd/relume

# Runtime: schlankes Image mit CA-Certs (für Cloud-Discovery via HTTPS)
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /relume /usr/local/bin/relume
VOLUME /data
WORKDIR /data
# Standard: serve. setup/link/discover via `docker compose run` aufrufen.
ENTRYPOINT ["relume"]
CMD ["serve", "-config", "/data/relume.json"]
