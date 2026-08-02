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
# Exit status:
#   0  the run passed every floor
#   3  a floor was breached: a real result, read the table
#   *  the bench did not run to completion (ssh, docker, config, flags, or a
#      refresh that could not fetch both halves of its recording). NOT an
#      accuracy result, and the script says so on stderr.
#
# --refresh replaces internal/matcher/testdata/corpus-replay.json.gz and its
# .meta.json sidecar together or not at all. A partial refresh reds push CI
# while looking like a completed one.
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
#
# The local branch is `bash -c`, not `-l`. A login shell was only ever needed to
# recover the ssh PATH, and re-sourcing the profile locally re-orders the PATH
# the operator invoked this script with, which silently picks a different binary
# than the one they can see.
on_host() {
	if [ -n "$HOST" ]; then
		# shellcheck disable=SC2016
		ssh "$HOST" "bash -lc $(printf '%q' "$*")"
	else
		bash -c "$*"
	fi
}

# quote_args turns arguments into one shell-safe string for on_host.
#
# on_host hands its argument to a shell either way (`bash -c` here, `bash -lc`
# on the far side of ssh), so EVERY word in it is re-parsed, and over ssh it is
# re-parsed twice. Quoting the container name and leaving the
# corpus path, the concurrency, the floors path and the commit spliced in raw
# meant four values reaching a shell unquoted, one of them (the commit) read out
# of an HTTP response. A container name with a space in it was the one case the
# original smoke test covered, and it was the one case already quoted.
quote_args() {
	local out="" a
	for a in "$@"; do
		out="$out $(printf '%q' "$a")"
	done
	printf '%s' "$out"
}

# fetch copies a file out of the container to a local path. Two hops when the
# bench host is remote, because `docker cp` lands on the host running docker.
#
# The remote staging path comes from mktemp: it used to be a fixed
# /tmp/forage-bench-<basename>, so two benches against the same host clobbered
# each other's half-copied files.
fetch() {
	local in_container="$1" local_path="$2" staging
	if [ -n "$HOST" ]; then
		# tail -n 1 because `bash -lc` is a login shell and a profile that
		# echoes anything would otherwise become part of the path.
		staging="$(on_host "mktemp /tmp/forage-bench-XXXXXXXX" | tail -n 1)" || return 1
		case "$staging" in
		/tmp/forage-bench-*) ;;
		*)
			echo "bench: could not stage a remote copy of $in_container (mktemp said: ${staging:-<nothing>})" >&2
			return 1
			;;
		esac
		on_host "docker cp $(quote_args "$CONTAINER:$in_container" "$staging")" || return 1
		scp -q "$HOST:$staging" "$local_path" || return 1
		on_host "rm -f $(quote_args "$staging")" || true
	else
		docker cp "$CONTAINER:$in_container" "$local_path" || return 1
	fi
}

# The commit actually being benched is whatever is in the running image, not
# whatever this checkout is at. Asking the daemon is the only way to know, and
# a wrong stamp is worse than none, so a failure here leaves it empty.
#
# The value is HTTP response body, and it ends up in an argument to a command a
# remote `bash -lc` will parse, so it is checked against the shape a version
# stamp has before it goes anywhere. A daemon that answered with something else
# is either not the daemon or not healthy, and neither is worth stamping into a
# fixture.
COMMIT="$(on_host "curl -fsS $(quote_args "$HEALTH")" 2>/dev/null | sed -n 's/.*"version":"\([^"]*\)".*/\1/p' || true)"
if [ -z "$COMMIT" ]; then
	echo "bench: could not read the running version from $HEALTH; the sidecar will not name a commit" >&2
elif ! printf '%s' "$COMMIT" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'; then
	echo "bench: $HEALTH returned a version that is not one ($COMMIT); the sidecar will not name a commit" >&2
	COMMIT=""
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
	on_host "docker exec $(quote_args "$CONTAINER" /build-corpus -out "$CORPUS")"
fi

# --limit=0 because the flag defaults to 500 and would silently bench a third
# of the corpus while the floors describe all of it.
bench_args=(--corpus="$CORPUS" --verify --gate --limit=0 --concurrency="$CONCURRENCY" --output-dir=/data)
if [ -n "${FORAGE_BENCH_FLOORS:-}" ]; then
	bench_args+=(--floors="$FORAGE_BENCH_FLOORS")
fi
if [ "$REFRESH" = "1" ]; then
	bench_args+=(--dump="$DUMP")
	# Only when there is one: an empty --dump-commit= is indistinguishable from
	# a stamped one in the sidecar, and the point of dropping an unreadable
	# version was to leave the field absent.
	if [ -n "$COMMIT" ]; then
		bench_args+=(--dump-commit="$COMMIT")
	fi
fi

echo "bench: running live Match+Verify (this takes ~26 minutes for ~1,570 entries)"
STATUS=0
on_host "docker exec $(quote_args "$CONTAINER" /matcher-bench "${bench_args[@]}")" || STATUS=$?

# 3 is matcher-bench's gate-breach status, and everything else it or the
# ssh/docker layer can exit with (255 no route, 127 no docker, 1 bad config, 2
# bad flags) means the bench did not produce a measurement at all. Reporting
# the second as "gate FAILED" sends someone to read accuracy numbers that were
# never computed.
case "$STATUS" in
0) ;;
3) echo "bench: GATE FAILED, floors breached. The table above says which." >&2 ;;
*) echo "bench: the bench did NOT complete (exit $STATUS). This is an ssh/docker/config failure, not an accuracy result." >&2 ;;
esac

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
		echo "bench: the run did not pass, so the fixture was NOT refreshed. A failing run must not become the new baseline." >&2
	else
		echo "bench: refreshing $FIXTURE_DIR from this run"
		# Both halves land in a staging directory first and move into place
		# together. They were fetched straight over the committed files, one
		# after the other, under `set -e`: a flaky scp on the second left a new
		# fixture beside the OLD sidecar, which is precisely the pair
		# TestCorpusFixtureMatchesTheLiveRun exists to reject, and it looked
		# like a completed refresh. The tree is now either fully refreshed or
		# untouched.
		STAGE="$(mktemp -d)"
		trap 'rm -rf "$STAGE"' EXIT
		if fetch "$DUMP" "$STAGE/corpus-replay.json.gz" &&
			fetch "/data/corpus-replay.meta.json" "$STAGE/corpus-replay.meta.json"; then
			mv "$STAGE/corpus-replay.json.gz" "$FIXTURE_DIR/corpus-replay.json.gz"
			mv "$STAGE/corpus-replay.meta.json" "$FIXTURE_DIR/corpus-replay.meta.json"
			echo "bench: fixture refreshed. Review the sidecar diff, then run:"
			echo "         go test ./internal/matcher/ -run 'Corpus|Fixture|Shipped|PipelineFloors' -count=1"
			echo "       Those floors are hand-set and are RATES, so a bigger corpus at"
			echo "       the same quality does not move them. If the new recording is"
			echo "       better, tighten them; if it is worse, find out why before you"
			echo "       loosen anything."
		else
			echo "bench: could not fetch both halves of the recording, so NOTHING was replaced." >&2
			echo "       The committed fixture and its sidecar are as they were. Re-run --refresh." >&2
			STATUS=1
		fi
	fi
fi

exit "$STATUS"
