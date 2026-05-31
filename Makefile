# forage / forager developer tasks.
.PHONY: ui build run test vet check

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
