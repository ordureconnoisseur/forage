# syntax=docker/dockerfile:1.7

# ── Build stage ────────────────────────────────────────────────────
FROM golang:1.26-alpine AS build
WORKDIR /src

ARG VERSION=docker

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO disabled keeps modernc.org/sqlite in pure-Go mode (smaller image,
# no glibc dependency in the final stage).
ENV CGO_ENABLED=0
RUN go build -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION}" \
    -o /out/forager .

# ── Runtime stage ──────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/forager /forager

# Persistent SQLite cache. Mount a volume here.
VOLUME ["/data"]
ENV FORAGER_DB_PATH=/data/forager.db

# Default listen addr — overridable. 0.0.0.0 is required because the
# container's loopback isn't reachable through the published port.
ENV FORAGER_LISTEN_ADDR=0.0.0.0:7979
EXPOSE 7979

USER nonroot:nonroot
ENTRYPOINT ["/forager"]
