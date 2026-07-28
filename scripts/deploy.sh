#!/usr/bin/env bash
# Deploy HEAD to a forage host running the docker-compose stack.
#
# The daemon's compose file builds from context `.` on the TARGET, so the
# target's tree is the build input. That makes "how the tree got there" a
# correctness question, not a convenience one.
#
# The old recipe (git archive | tar -xzf over the existing tree) overwrites
# but never deletes, so anything removed in git lingered on the target and
# kept getting compiled. On 2026-07-28 that produced a build failure (a
# deleted package still present) and, worse, revealed a source file that had
# been untracked by git and compiled into every deployed binary for an
# unknown time, plus a whole stale pre-`plugin/` src tree. The deployed
# artifact was not what the repository said it was.
#
# So: delete exactly what the archive owns, then extract. The archive
# declares its own top-level entries, so nothing else is ever touched, and
# the target's gitignored runtime files (data/, .env, docker-compose.yml,
# tailscale-state/) are outside that set by construction. Drift becomes
# impossible rather than detectable.
#
# Usage:  scripts/deploy.sh
# Env:    FORAGE_DEPLOY_HOST (default: mini)
#         FORAGE_DEPLOY_DIR  (default: forager, relative to the remote HOME)
#         FORAGE_HEALTH_URL  (default: http://127.0.0.1:7979/healthz)
set -euo pipefail

HOST="${FORAGE_DEPLOY_HOST:-mini}"
DIR="${FORAGE_DEPLOY_DIR:-forager}"
HEALTH="${FORAGE_HEALTH_URL:-http://127.0.0.1:7979/healthz}"

cd "$(dirname "$0")/.."

# Deploy what is committed. A dirty tree means the SHA stamped into the
# binary would describe something other than what shipped, and the version
# check at the end would be verifying a lie.
if ! git diff-index --quiet HEAD --; then
	echo "deploy: working tree is dirty; commit or stash first." >&2
	git status --short >&2
	exit 1
fi

SHA="$(git rev-parse HEAD)"
TARBALL="/tmp/forage-deploy-${SHA:0:12}.tar.gz"
echo "deploy: $SHA → $HOST:~/$DIR"

git archive --format=tar.gz -o "$TARBALL" HEAD
scp -q "$TARBALL" "$HOST:$TARBALL"

# Remote half. Fed over stdin so nothing needs escaping twice, and run under
# `bash -lc` because docker is not on the non-interactive ssh PATH.
ssh "$HOST" "bash -lc 'bash -s -- \"$SHA\" \"$DIR\" \"$TARBALL\" \"$HEALTH\"'" <<'REMOTE'
set -euo pipefail
SHA="$1"; DIR="$2"; TARBALL="$3"; HEALTH="$4"
cd "$HOME/$DIR"

# What the archive owns, straight from the archive. Everything here is
# reconstituted by the extract below, so removing it first is safe; anything
# NOT here (data/, .env, docker-compose.yml, tailscale-state/) is untouched.
# Read loop rather than `mapfile`: macOS ships bash 3.2, which has no
# mapfile, and the remote half runs under the target's /bin/bash.
OWNED=()
while IFS= read -r entry; do
	[ -n "$entry" ] && OWNED+=("$entry")
done < <(tar -tzf "$TARBALL" | cut -d/ -f1 | sort -u)
if [ "${#OWNED[@]}" -eq 0 ]; then
	echo "deploy: archive lists no entries; refusing to touch anything" >&2
	exit 1
fi
for e in "${OWNED[@]}"; do
	# Paranoia: an absolute, empty, or traversing entry would make the rm
	# below reach outside the deploy directory.
	case "$e" in
		"" | "." | ".." | /* | *..*)
			echo "deploy: refusing suspicious archive entry: '$e'" >&2
			exit 1
			;;
	esac
done
printf 'deploy: replacing %d archive-owned entries\n' "${#OWNED[@]}"
rm -rf -- "${OWNED[@]}"
tar -xzf "$TARBALL"

# Belt and braces: every source file in the build context must be one the
# archive just put there. If this fires, the replace above missed something
# and the build would compile code the repository does not have — the exact
# failure this script exists to make impossible.
tar -tzf "$TARBALL" | grep -E '\.(go|ts|tsx|css)$' | sort >/tmp/forage-archive-src.txt
find . -type f \( -name '*.go' -o -name '*.ts' -o -name '*.tsx' -o -name '*.css' \) \
	-not -path './node_modules/*' -not -path './plugin/node_modules/*' \
	-not -path './data/*' | sed 's|^\./||' | sort >/tmp/forage-context-src.txt
EXTRA="$(comm -13 /tmp/forage-archive-src.txt /tmp/forage-context-src.txt)"
if [ -n "$EXTRA" ]; then
	echo "deploy: build context holds source files not in the commit:" >&2
	echo "$EXTRA" >&2
	exit 1
fi
printf 'deploy: build context clean (%s source files, all from the commit)\n' \
	"$(wc -l </tmp/forage-archive-src.txt | tr -d ' ')"
rm -f "$TARBALL" /tmp/forage-archive-src.txt /tmp/forage-context-src.txt

# Unpiped, so a build failure is a failure. Piping this through tail (or
# anything) hands back the pipe's exit status and lets `up -d` proceed
# against the stale image, which has happened.
VERSION="$SHA" docker compose build forager
docker compose up -d --no-deps forager

# The deploy is not done until the running daemon says it is this commit.
for i in $(seq 1 30); do
	sleep 2
	RUNNING="$(curl -fsS "$HEALTH" 2>/dev/null | sed -n 's/.*"version":"\([^"]*\)".*/\1/p' || true)"
	if [ "$RUNNING" = "$SHA" ]; then
		echo "deploy: verified, /healthz reports $RUNNING"
		exit 0
	fi
done
echo "deploy: FAILED verification — /healthz reports '${RUNNING:-<nothing>}', expected $SHA" >&2
exit 1
REMOTE

rm -f "$TARBALL"
echo "deploy: done"
