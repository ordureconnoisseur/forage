#!/usr/bin/env bash
# Full-pipeline matcher regression bench: build a corpus from the daemon's
# confirmed grabs, run live Match+Verify over it, fail on regression.
#
# WHY THIS IS A SCRIPT AND NOT A CI JOB
#
# The per-push gate (internal/matcher/corpus_gate_test.go) replays a frozen
# candidate set, which guards Verify and nothing else: a regression in query
# construction, retrieval or the confidence weights cannot show up, because the
# candidates were recorded before the change. The headline accuracy figure is
# Match AND Verify together, so half of it had no gate at all.
#
# Closing that needs the live pipeline, and the live pipeline needs three
# things a GitHub-hosted runner cannot have:
#
#   1. StashDB credentials AND ~26 minutes of round trips for 1,570 entries;
#   2. the daemon's SQLite performer/studio caches, which is what
#      matcher.New loads and what makes entity recognition work at all;
#   3. the daemon's /grabs history, which is where the corpus comes from,
#      and which lives on a tailnet host, not the public internet.
#
# So this runs against the maintainer's own instance, weekly, either by hand or
# from a self-hosted runner. See docs/matcher-accuracy.md.
#
# Usage:
#   scripts/matcher-bench.sh              # measure and gate
#   scripts/matcher-bench.sh --refresh    # ... and re-record the committed
#                                         #     replay fixture from this run
#
# Env:
#   FORAGE_BENCH_HOST         ssh host running the forage container. Empty
#                             (default) means docker is local.
#   FORAGE_BENCH_CONTAINER    container name (default: forager)
#   FORAGE_BENCH_CORPUS       corpus path INSIDE the container
#                             (default: /data/corpus.yaml)
#   FORAGE_BENCH_CONCURRENCY  parallel Match calls (default: 8)
#   FORAGE_BENCH_REUSE_CORPUS set to 1 to skip the corpus rebuild and use
#                             whatever is already at FORAGE_BENCH_CORPUS
#   FORAGE_BENCH_FLOORS       floors JSON path inside the container; default
#                             is the set compiled into the bench binary
#   FORAGE_HEALTH_URL         daemon health endpoint on the bench host, read
#                             only to stamp the benched commit into the
#                             fixture sidecar (default:
#                             http://127.0.0.1:7979/healthz)
set -euo pipefail

HOST="${FORAGE_BENCH_HOST:-}"
CONTAINER="${FORAGE_BENCH_CONTAINER:-forager}"
CORPUS="${FORAGE_BENCH_CORPUS:-/data/corpus.yaml}"
CONCURRENCY="${FORAGE_BENCH_CONCURRENCY:-8}"
HEALTH="${FORAGE_HEALTH_URL:-http://127.0.0.1:7979/healthz}"
DUMP="/data/corpus-replay.json.gz"

REFRESH=0
case "${1:-}" in
	--refresh) REFRESH=1 ;;
	"") ;;
	*)
		echo "usage: $0 [--refresh]" >&2
		exit 2
		;;
esac

cd "$(dirname "$0")/.."
FIXTURE_DIR="internal/matcher/testdata"

# One quoting level: printf %q makes the whole command a single safe word, and
# `bash -lc` because docker is not on the non-interactive ssh PATH (the same
# reason scripts/deploy.sh needs it).
on_host() {
	if [ -n "$HOST" ]; then
		# shellcheck disable=SC2016
		ssh "$HOST" "bash -lc $(printf '%q' "$*")"
	else
		bash -lc "$*"
	fi
}

# fetch copies a file out of the container to a local path. Two hops when the
# bench host is remote, because `docker cp` lands on the host running docker.
fetch() {
	local in_container="$1" local_path="$2" staging
	if [ -n "$HOST" ]; then
		staging="/tmp/forage-bench-$(basename "$in_container")"
		on_host "docker cp $(printf '%q' "$CONTAINER:$in_container") $(printf '%q' "$staging")" || return 1
		scp -q "$HOST:$staging" "$local_path" || return 1
		on_host "rm -f $(printf '%q' "$staging")" || true
	else
		docker cp "$CONTAINER:$in_container" "$local_path" || return 1
	fi
}

# The commit actually being benched is whatever is in the running image, not
# whatever this checkout is at. Asking the daemon is the only way to know, and
# a wrong stamp is worse than none, so a failure here leaves it empty.
COMMIT="$(on_host "curl -fsS $(printf '%q' "$HEALTH")" 2>/dev/null | sed -n 's/.*"version":"\([^"]*\)".*/\1/p' || true)"
if [ -z "$COMMIT" ]; then
	echo "bench: could not read the running version from $HEALTH; the sidecar will not name a commit" >&2
fi

if [ "${FORAGE_BENCH_REUSE_CORPUS:-0}" = "1" ]; then
	echo "bench: reusing the corpus already at $CORPUS"
else
	echo "bench: building corpus from confirmed grabs → $CORPUS"
	# -include-downloads is NOT passed, and must never be. It folds qBit/SAB
	# download names into the corpus: post-renamed FILENAMES, which the
	# matcher never sees in production (it scores Prowlarr release titles at
	# search time). Every threshold in the verifier was originally fitted on
	# that corpus, and the reported accuracy was wrong by a factor for weeks
	# because of it.
	on_host "docker exec $(printf '%q' "$CONTAINER") /build-corpus -out $(printf '%q' "$CORPUS")"
fi

# --limit=0 because the flag defaults to 500 and would silently bench a third
# of the corpus while the floors describe all of it.
BENCH_ARGS="--corpus=$CORPUS --verify --gate --limit=0 --concurrency=$CONCURRENCY --output-dir=/data"
if [ -n "${FORAGE_BENCH_FLOORS:-}" ]; then
	BENCH_ARGS="$BENCH_ARGS --floors=$FORAGE_BENCH_FLOORS"
fi
if [ "$REFRESH" = "1" ]; then
	BENCH_ARGS="$BENCH_ARGS --dump=$DUMP --dump-commit=$COMMIT"
fi

echo "bench: running live Match+Verify (this takes ~26 minutes for ~1,570 entries)"
STATUS=0
on_host "docker exec $(printf '%q' "$CONTAINER") /matcher-bench $BENCH_ARGS" || STATUS=$?

# Always fetch the per-failure rows, pass or fail. A gate that says "recall
# dropped 3 points" and nothing else sends you back to re-run 26 minutes just to
# find out which releases; the CSV is what `matcher-bench --explain` takes as
# input. Gitignored: it holds real release names and scene ids.
fetch "/data/verify.failures.csv" "verify.failures.csv" 2>/dev/null &&
	echo "bench: failure rows in ./verify.failures.csv (inspect one with: docker exec $CONTAINER /matcher-bench --explain='<release>' --expect='<scene id>')" ||
	echo "bench: no failure CSV to fetch" >&2

if [ "$REFRESH" = "1" ]; then
	if [ "$STATUS" -ne 0 ]; then
		# Recording a regressed run as the new baseline is how a gate stops
		# being a gate: the frozen replay would then assert the broken
		# behaviour and pass forever. Fix the regression, or re-derive the
		# floors deliberately, then refresh.
		echo "bench: gate FAILED, so the fixture was NOT refreshed. A failing run must not become the new baseline." >&2
	else
		echo "bench: refreshing $FIXTURE_DIR from this run"
		fetch "$DUMP" "$FIXTURE_DIR/corpus-replay.json.gz"
		fetch "/data/corpus-replay.meta.json" "$FIXTURE_DIR/corpus-replay.meta.json"
		echo "bench: fixture refreshed. Review the sidecar diff, then run:"
		echo "         go test ./internal/matcher/ -run Corpus -count=1"
		echo "       The replay floors in corpus_gate_test.go are hand-set and may"
		echo "       now be loose against the new recording; ratchet them if so."
	fi
fi

exit "$STATUS"
