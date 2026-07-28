module github.com/ordureconnoisseur/forager

go 1.26.3

// Build with a toolchain carrying the crypto/tls fix for GO-2026-5856, so
// the govulncheck gate in CI is clean on the standard library too. The `go`
// line above stays at the real minimum: this raises what we BUILD with, not
// what a consumer needs to compile the module.
toolchain go1.26.5

require (
	github.com/go-chi/chi/v5 v5.3.0
	golang.org/x/crypto v0.52.0
	golang.org/x/text v0.39.0
	modernc.org/sqlite v1.50.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.45.0 // indirect
	modernc.org/libc v1.72.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
