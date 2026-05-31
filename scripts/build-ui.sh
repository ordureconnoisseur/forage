#!/usr/bin/env bash
# Build the forage plugin SPA and embed it into the daemon, so forager can
# serve the standalone web app at `/`. Run after any change under plugin/src.
#
# The daemon's served UI is internal/api/ui/index.html (//go:embed). We keep
# the built bundle committed there so a plain `go build` / `go run .` works
# without a Node toolchain; this script regenerates it. Docker rebuilds it
# fresh from source on every release (see Dockerfile's `ui` stage).
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
cd "$here/plugin"

# Install only when missing — fast no-op on repeat local runs, reproducible
# from the committed lockfile when it does run.
[ -d node_modules ] || npm ci

npm run build
cp dist/index.html "$here/internal/api/ui/index.html"
echo "embedded plugin/dist/index.html → internal/api/ui/index.html ($(wc -c < "$here/internal/api/ui/index.html") bytes)"
