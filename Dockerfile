# syntax=docker/dockerfile:1.7

# ── UI stage ───────────────────────────────────────────────────────
# Build the plugin SPA fresh from source so the released image always
# serves the current standalone app at /, regardless of what's committed
# to internal/api/ui/index.html.
FROM node:22-alpine AS ui
WORKDIR /ui
COPY plugin/package.json plugin/package-lock.json ./
RUN npm ci
COPY plugin/ ./
RUN npm run build

# ── Build stage ────────────────────────────────────────────────────
FROM golang:1.26-alpine AS build
WORKDIR /src

ARG VERSION=docker

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Overwrite the committed bundle with the freshly-built one before embedding.
COPY --from=ui /ui/dist/index.html internal/api/ui/index.html

# CGO disabled keeps modernc.org/sqlite in pure-Go mode (smaller image,
# no glibc dependency in the final stage).
ENV CGO_ENABLED=0
RUN go build -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION}" \
    -o /out/forager .

# build-corpus is a one-shot tool that walks qBit + SAB history and
# emits a YAML matcher-test corpus. Ships in the same image so it
# can be invoked via `docker exec forager /build-corpus ...` with
# the daemon's env (host.docker.internal resolution + .env credentials).
RUN go build -trimpath \
    -ldflags="-s -w" \
    -o /out/build-corpus ./tools/build-corpus

# matcher-bench runs the production matcher against either the user's
# Stash library OR a pre-built corpus YAML, reporting P@1/P@3/P@10.
RUN go build -trimpath \
    -ldflags="-s -w" \
    -o /out/matcher-bench ./tools/matcher-bench

# refile-unsorted re-files grabs the placer dropped into Unsorted back under
# their scene's performer. Dry-run by default; --apply moves files, so it is a
# deliberate one-shot rather than anything the daemon runs.
RUN go build -trimpath     -ldflags="-s -w"     -o /out/refile-unsorted ./tools/refile-unsorted

# ── Runtime stage ──────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/forager /forager
COPY --from=build /out/build-corpus /build-corpus
COPY --from=build /out/matcher-bench /matcher-bench
COPY --from=build /out/refile-unsorted /refile-unsorted

# Persistent SQLite cache. Mount a volume here.
VOLUME ["/data"]
ENV FORAGER_DB_PATH=/data/forager.db

# Default listen addr — overridable. 0.0.0.0 is required because the
# container's loopback isn't reachable through the published port.
ENV FORAGER_LISTEN_ADDR=0.0.0.0:7979
EXPOSE 7979

USER nonroot:nonroot
ENTRYPOINT ["/forager"]
