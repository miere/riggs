package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder captures what the harness would have executed.
//
// Guarded, because one of these is shared by two concurrent runs in run_test.go
// — which is the behaviour that test is about.
type recorder struct {
	mu   sync.Mutex
	dir  string
	argv []string
	out  string
	err  error
	// block holds the fake process open, so a timeout can be exercised without
	// a real one.
	block time.Duration
}

// lastArgv is the argv of the most recent run.
func (r *recorder) lastArgv() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.argv...)
}

// lastPrompt is the final argument of the most recent run: the prompt.
func (r *recorder) lastPrompt() string {
	argv := r.lastArgv()
	if len(argv) == 0 {
		return ""
	}
	return argv[len(argv)-1]
}

func (r *recorder) exec(ctx context.Context, dir string, argv []string) ([]byte, error) {
	r.mu.Lock()
	r.dir, r.argv = dir, argv
	out, err, block := r.out, r.err, r.block
	r.mu.Unlock()
	if block > 0 {
		select {
		case <-ctx.Done():
			return []byte(out), ctx.Err()
		case <-time.After(block):
		}
	}
	return []byte(out), err
}

func harness(t *testing.T, command string, rec *recorder) *Harness {
	t.Helper()
	h := New(command, "/work", time.Minute)
	if h == nil {
		t.Fatalf("New(%q) = nil", command)
	}
	return h.WithExec(rec.exec)
}

// A known harness gets its own prompt flag. Handing `claude` a bare argument
// makes it a session prompt rather than a one-shot, which is a different tool.
func TestArgvGivesAKnownHarnessItsFlag(t *testing.T) {
	rec := &recorder{}
	got := harness(t, "claude", rec).Argv("review it")
	want := []string{"claude", "-p", "review it"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Argv = %v, want %v", got, want)
	}
}

// Matched on the base name, so an absolute path is the same harness.
func TestArgvMatchesTheHarnessByBaseName(t *testing.T) {
	got := harness(t, "/opt/homebrew/bin/claude", &recorder{}).Argv("go")
	want := []string{"/opt/homebrew/bin/claude", "-p", "go"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Argv = %v, want %v", got, want)
	}
}

// The admin's own arguments are kept, and the prompt flag goes after them: a
// flag's value has to be adjacent to it, and interleaving somebody else's
// arguments is not ours to do.
func TestArgvKeepsTheConfiguredArguments(t *testing.T) {
	got := harness(t, "claude --model opus", &recorder{}).Argv("go")
	want := []string{"claude", "--model", "opus", "-p", "go"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Argv = %v, want %v", got, want)
	}
}

// Anything unknown takes the prompt as its first argument, which is the
// documented contract and what most one-shot CLIs do. Guessing a flag from a
// name is how a review prompt gets read as a filename.
func TestArgvHandsACustomCommandTheBarePrompt(t *testing.T) {
	got := harness(t, "/usr/local/bin/my-reviewer --json", &recorder{}).Argv("go")
	want := []string{"/usr/local/bin/my-reviewer", "--json", "go"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Argv = %v, want %v", got, want)
	}
}

// "Not configured" is the ordinary state of this feature, not an error. Every
// caller already renders differently for it.
func TestAnEmptyCommandIsNoHarness(t *testing.T) {
	if h := New("   ", "/work", time.Minute); h != nil {
		t.Fatalf("New(blank) = %v, want nil", h)
	}
	var h *Harness
	if h.Program() != "" || h.Dir() != "" || h.Argv("x") != nil {
		t.Fatal("a nil harness answered as though it were configured")
	}
	if _, err := h.Run(context.Background(), "x"); err == nil {
		t.Fatal("a nil harness ran something")
	}
}

// The working directory is the setting that decides whether this works at all:
// Claude Code reads the project it is standing in.
func TestRunExecutesInTheConfiguredDirectory(t *testing.T) {
	rec := &recorder{out: "  done  "}
	result, err := harness(t, "claude", rec).Run(context.Background(), "review o/r#7")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rec.mu.Lock()
	dir := rec.dir
	rec.mu.Unlock()
	if dir != "/work" {
		t.Fatalf("ran in %q, want /work", dir)
	}
	if result.Output != "done" {
		t.Fatalf("Output = %q, want it trimmed", result.Output)
	}
}

// A failed run keeps its output. The whole value of the failure line is the
// tail of what the harness said before it gave up.
func TestRunReportsFailureWithItsOutput(t *testing.T) {
	rec := &recorder{out: "boom", err: errors.New("exit status 1")}
	result, err := harness(t, "claude", rec).Run(context.Background(), "go")
	if err == nil {
		t.Fatal("a failing harness reported success")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Fatalf("the error does not name the harness: %v", err)
	}
	if result.Output != "boom" {
		t.Fatalf("Output = %q", result.Output)
	}
	if result.TimedOut {
		t.Fatal("an ordinary failure was reported as a timeout")
	}
}

// A hung harness and one that is thinking hard look identical from here, which
// is exactly why there is a bound. The two need different words: one is a
// review that went wrong, the other may still have been going.
func TestRunTimesOutAndSaysSo(t *testing.T) {
	rec := &recorder{block: time.Minute}
	h := New("claude", "", time.Millisecond).WithExec(rec.exec)
	result, err := h.Run(context.Background(), "go")
	if err == nil {
		t.Fatal("a run past its bound reported success")
	}
	if !result.TimedOut {
		t.Fatalf("TimedOut = false on a timeout: %v", err)
	}
	if !strings.Contains(err.Error(), "gave up") {
		t.Fatalf("the timeout does not read as one: %v", err)
	}
}
