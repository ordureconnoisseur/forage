# forage / forager developer tasks.
.PHONY: ui build run test vet check bench bench-refresh

# Build the plugin SPA and embed it into the daemon (serves at /).
ui:
	bash scripts/build-ui.sh

# Build the daemon. Run `make ui` first if you changed the plugin.
build:
	CGO_ENABLED=0 go build -o forager .

# Run the daemon locally.
run:
	CGO_ENABLED=0 go run .

test:
	go test ./...

vet:
	go vet ./...

# Everything CI cares about.
check: vet test

# Full-pipeline matcher accuracy: live Match+Verify over a corpus built from
# the daemon's confirmed grabs, gated on recorded floors. Needs a running
# forage instance (StashDB credentials, the daemon's caches, its grab history),
# so it is a weekly job against your own box and NOT part of `check`. The
# per-push gate replays frozen candidates and guards only Verify.
# See docs/matcher-accuracy.md.
bench:
	bash scripts/matcher-bench.sh

# Same run, and re-record the committed replay fixture from it, so the frozen
# per-push gate keeps resembling current matcher input. Refuses to write the
# fixture if the run breached a floor: a regressed run must never become the
# baseline.
bench-refresh:
	bash scripts/matcher-bench.sh --refresh
