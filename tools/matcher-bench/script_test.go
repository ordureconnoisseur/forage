package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/matcher-bench.sh, driven against fake docker/curl/scp.
//
// The script is `make bench` and `make bench-refresh`, i.e. the deliverable,
// and it had never been executed end to end: its failure modes (a half-applied
// refresh, unquoted values reaching a remote shell) were found by reading. This
// runs it, with the ssh/docker layer replaced by shims that record their argv,
// against a throwaway copy of the repo layout.
//
// What it proves: argument construction, the refresh's staging and rollback,
// and the exit-status handling. What it does NOT prove: that `docker exec`,
// `docker cp` and `scp` behave as assumed against a real remote host. Only a
// live run settles that, and the local-docker path (FORAGE_BENCH_HOST unset) is
// the one exercised here.

// benchHarness is a throwaway tree shaped like the repo, with the script in it
// and a fake PATH in front.
type benchHarness struct {
	root    string // holds scripts/ and internal/matcher/testdata/
	binDir  string
	argvLog string // one line per fake invocation
}

func newBenchHarness(t *testing.T) *benchHarness {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		// The script is bash and the shims are shell scripts on a PATH. A
		// machine without bash cannot run this, and skipping is honest: CI is
		// ubuntu-latest, where it always runs.
		t.Skip("no bash on PATH; this harness needs one")
	}
	h := &benchHarness{root: t.TempDir()}
	h.binDir = filepath.Join(h.root, "fakebin")
	h.argvLog = filepath.Join(h.root, "argv.log")

	mustMkdir(t, filepath.Join(h.root, "scripts"))
	mustMkdir(t, filepath.Join(h.root, "internal", "matcher", "testdata"))
	mustMkdir(t, h.binDir)

	src, err := os.ReadFile(filepath.Join("..", "..", "scripts", "matcher-bench.sh"))
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(h.root, "scripts", "matcher-bench.sh"), string(src), 0o755)

	// The committed fixture and sidecar, standing in for the real ones. The
	// refresh must replace both or neither.
	mustWrite(t, h.fixture(), "OLD FIXTURE", 0o644)
	mustWrite(t, h.sidecar(), "OLD SIDECAR", 0o644)
	return h
}

func (h *benchHarness) fixture() string {
	return filepath.Join(h.root, "internal", "matcher", "testdata", "corpus-replay.json.gz")
}

func (h *benchHarness) sidecar() string {
	return filepath.Join(h.root, "internal", "matcher", "testdata", "corpus-replay.meta.json")
}

// fake installs a shim that logs its argv one-per-line (delimited, so a value
// containing a space is distinguishable from two values) and then runs body.
func (h *benchHarness) fake(t *testing.T, name, body string) {
	t.Helper()
	script := fmt.Sprintf(`#!/usr/bin/env bash
{
  printf '%s'
  for a in "$@"; do printf ' <%%s>' "$a"; done
  printf '\n'
} >> %q
%s
`, name+":", h.argvLog, body)
	mustWrite(t, filepath.Join(h.binDir, name), script, 0o755)
}

func (h *benchHarness) run(t *testing.T, env []string, args ...string) (string, int) {
	t.Helper()
	script := filepath.ToSlash(filepath.Join(h.root, "scripts", "matcher-bench.sh"))
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = h.root
	cmd.Env = append(os.Environ(),
		"PATH="+h.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FORAGE_BENCH_HOST=",
	)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if err != nil {
		if !asExitError(err, &ee) {
			t.Fatalf("running the script: %v\n%s", err, out)
		}
		code = ee.ExitCode()
	}
	return string(out), code
}

func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

func (h *benchHarness) argv(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(h.argvLog)
	if err != nil {
		return ""
	}
	return string(b)
}

// The refresh replaces the fixture and its sidecar, or neither.
//
// It used to be two bare `fetch` calls in a row under `set -euo pipefail`: a
// flaky second copy aborted the script with a NEW fixture beside the OLD
// sidecar, which is the exact pair TestCorpusFixtureMatchesTheLiveRun rejects,
// and it looked from the outside like a completed refresh.
func TestBenchScriptRefreshIsAllOrNothing(t *testing.T) {
	for _, tt := range []struct {
		name        string
		failOn      string // the container path whose copy fails
		wantFixture string
		wantSidecar string
		wantCode    int
	}{
		{"both copies succeed", "", "NEW FIXTURE", "NEW SIDECAR", 0},
		{"the sidecar copy fails", "/data/corpus-replay.meta.json", "OLD FIXTURE", "OLD SIDECAR", 1},
		{"the fixture copy fails", "/data/corpus-replay.json.gz", "OLD FIXTURE", "OLD SIDECAR", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newBenchHarness(t)
			h.fake(t, "curl", `printf '{"version":"abc1234"}'`)
			h.fake(t, "docker", fmt.Sprintf(`
case "$1 $2" in
  "exec forager")
    case "$3" in
      /build-corpus) exit 0 ;;
      /matcher-bench) exit 0 ;;
    esac ;;
esac
if [ "$1" = "cp" ]; then
  src="${2#forager:}"
  if [ -n %q ] && [ "$src" = %q ]; then exit 1; fi
  case "$src" in
    /data/corpus-replay.json.gz) printf 'NEW FIXTURE' > "$3" ;;
    /data/corpus-replay.meta.json) printf 'NEW SIDECAR' > "$3" ;;
    /data/verify.failures.csv) printf 'kind,release\n' > "$3" ;;
    *) exit 1 ;;
  esac
  exit 0
fi
exit 0
`, tt.failOn, tt.failOn))

			out, code := h.run(t, nil, "--refresh")
			if code != tt.wantCode {
				t.Errorf("exit %d, want %d\n%s", code, tt.wantCode, out)
			}
			if got := readFile(t, h.fixture()); got != tt.wantFixture {
				t.Errorf("fixture is %q, want %q: a partial refresh was committed to the tree\n%s", got, tt.wantFixture, out)
			}
			if got := readFile(t, h.sidecar()); got != tt.wantSidecar {
				t.Errorf("sidecar is %q, want %q\n%s", got, tt.wantSidecar, out)
			}
		})
	}
}

// Every value the script splices into a command reaches a shell, because
// on_host hands the lot to `bash -lc`. Four of them used to go in unquoted.
//
// The corpus path here contains a space and a semicolon; if quoting is wrong it
// arrives as several arguments, or runs as a second command.
func TestBenchScriptQuotesEveryValueItSplices(t *testing.T) {
	h := newBenchHarness(t)
	h.fake(t, "curl", `printf '{"version":"abc1234"}'`)
	h.fake(t, "docker", `exit 0`)

	corpus := "/data/my corpus; touch /tmp/forage-bench-pwned.yaml"
	out, code := h.run(t, []string{
		"FORAGE_BENCH_CORPUS=" + corpus,
		"FORAGE_BENCH_CONCURRENCY=8",
		"FORAGE_BENCH_FLOORS=/data/floors file.json",
	})
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	argv := h.argv(t)
	// One argv element, delimited, containing the whole path: proof it did not
	// word-split and did not run as a second command.
	for _, want := range []string{
		"<--corpus=" + corpus + ">",
		"<--floors=/data/floors file.json>",
		"<-out> <" + corpus + ">",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv does not contain %q, so the value was split or mangled:\n%s", want, argv)
		}
	}
	if _, err := os.Stat("/tmp/forage-bench-pwned.yaml"); err == nil {
		os.Remove("/tmp/forage-bench-pwned.yaml")
		t.Error("the corpus path ran as a command: quoting is not doing its job")
	}
}

// The same, over the ssh path, which is where the finding actually bit: with
// FORAGE_BENCH_HOST set, every spliced value is parsed by a shell on the far
// side as well as this one.
//
// The fake ssh models the part that matters: real ssh joins its command
// arguments with spaces and the far end runs the result through a shell, so a
// value that survives one round of quoting and not two shows up here. It is not
// a login shell, unlike the real remote, which is the one way this differs.
func TestBenchScriptQuotesForTheRemoteShellToo(t *testing.T) {
	h := newBenchHarness(t)
	h.fake(t, "curl", `printf '{"version":"abc1234"}'`)
	h.fake(t, "docker", `exit 0`)
	h.fake(t, "ssh", `shift; sh -c "$*"`)

	corpus := "/data/my corpus; touch /tmp/forage-bench-pwned3.yaml"
	out, code := h.run(t, []string{
		"FORAGE_BENCH_HOST=bench-host",
		"FORAGE_BENCH_CORPUS=" + corpus,
	})
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(h.argv(t), "<--corpus="+corpus+">") {
		t.Errorf("the corpus path did not survive two shells intact:\n%s", h.argv(t))
	}
	if _, err := os.Stat("/tmp/forage-bench-pwned3.yaml"); err == nil {
		os.Remove("/tmp/forage-bench-pwned3.yaml")
		t.Error("a value reaching the remote shell ran as a command there")
	}
}

// The commit stamp is an HTTP response body that ends up as an argument to a
// command a remote shell parses. A daemon (or something answering for it) that
// returns anything other than a version stamp must not have that value used.
func TestBenchScriptRefusesAVersionThatIsNotOne(t *testing.T) {
	h := newBenchHarness(t)
	h.fake(t, "curl", `printf '{"version":"7.0; touch /tmp/forage-bench-pwned2"}'`)
	h.fake(t, "docker", `
if [ "$1" = "cp" ]; then printf 'NEW' > "$3"; fi
exit 0
`)

	out, code := h.run(t, nil, "--refresh")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "not one") {
		t.Errorf("the script did not say it was dropping the bogus version:\n%s", out)
	}
	if strings.Contains(h.argv(t), "--dump-commit") {
		t.Errorf("a rejected version was still stamped into the sidecar:\n%s", h.argv(t))
	}
	if _, err := os.Stat("/tmp/forage-bench-pwned2"); err == nil {
		os.Remove("/tmp/forage-bench-pwned2")
		t.Error("the version string ran as a command")
	}
}

// A gate breach and an ssh/docker failure both exit non-zero, and reporting the
// second as the first sends someone to read accuracy numbers that were never
// computed. matcher-bench exits 3 for a breached floor; everything else is a
// run that did not happen.
func TestBenchScriptDistinguishesAGateBreachFromAFailedRun(t *testing.T) {
	for _, tt := range []struct {
		name     string
		exit     int
		wantSaid string
		wantGone string
	}{
		{"floors breached", 3, "GATE FAILED", "did NOT complete"},
		{"docker not found", 127, "did NOT complete", "GATE FAILED"},
		{"no route to host", 255, "did NOT complete", "GATE FAILED"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newBenchHarness(t)
			h.fake(t, "curl", `printf '{"version":"abc1234"}'`)
			h.fake(t, "docker", fmt.Sprintf(`
if [ "$1 $3" = "exec /matcher-bench" ]; then exit %d; fi
if [ "$1" = "cp" ]; then printf 'x' > "$3"; fi
exit 0
`, tt.exit))
			out, code := h.run(t, nil)
			if code != tt.exit {
				t.Errorf("exit %d, want %d (the script must pass the bench's status through)\n%s", code, tt.exit, out)
			}
			if !strings.Contains(out, tt.wantSaid) {
				t.Errorf("output does not say %q:\n%s", tt.wantSaid, out)
			}
			if strings.Contains(out, tt.wantGone) {
				t.Errorf("output wrongly says %q:\n%s", tt.wantGone, out)
			}
		})
	}
}

// A run that did not pass must not become the new baseline: the frozen replay
// would then assert the broken behaviour and pass forever.
func TestBenchScriptDoesNotRefreshFromAFailedRun(t *testing.T) {
	h := newBenchHarness(t)
	h.fake(t, "curl", `printf '{"version":"abc1234"}'`)
	h.fake(t, "docker", `
if [ "$1 $3" = "exec /matcher-bench" ]; then exit 3; fi
if [ "$1" = "cp" ]; then printf 'NEW' > "$3"; fi
exit 0
`)
	out, code := h.run(t, nil, "--refresh")
	if code != 3 {
		t.Errorf("exit %d, want 3\n%s", code, out)
	}
	if got := readFile(t, h.fixture()); got != "OLD FIXTURE" {
		t.Errorf("a breached run was recorded as the new baseline: fixture is %q\n%s", got, out)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}
