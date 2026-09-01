// Package ai runs a local AI harness — Claude Code, or anything else that takes
// a prompt — on the machine Riggs is installed on.
//
// It is what separates "Run Code Review" from "Ask for Code Review", and "Run
// AI Assistance" from "Ask for SME Assistance". The two asks post a message and
// stop; a human reads it and decides. These start a process that does the work.
// Riggs conflated them for a long time, in the worst direction: an option
// labelled "Ask for AI Assistance" that quietly tagged a colleague.
//
// The package name is deliberately the one Phase 21 retired. That `internal/ai`
// shelled out to `claude -p` for a one-paragraph card summary and was removed
// because a summary is not worth 8.6 seconds on the click path, a hard
// dependency on a local binary, and output that changed between renders. None
// of those objections applies here: this IS the work, nobody is waiting on a
// render, and a machine without the binary is told the option is off rather
// than being shown one that cannot fire.
//
// It holds no credential. The harness carries its own auth, exactly as `gh`
// does for GitHub, which keeps Riggs out of the business of storing another key.
package ai

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Exec executes the harness. It is the seam that keeps `exec.Command` out of
// the call sites, so every path that reaches a harness is fakeable — the rule
// §11 applies to `gh` and applied to the retired summariser.
//
// Named Exec rather than Runner, which is what the equivalent seam is called
// everywhere else in this codebase: Runner here is the thing that runs an ITEM
// and reports on it (run.go), and that is the name a reader of this package
// will reach for first.
//
// It returns the combined output: a harness that failed says why on whichever
// stream it feels like, and a reader looking at "it failed" wants whichever one
// that was.
type Exec func(ctx context.Context, dir string, argv []string) (output []byte, err error)

// Harness is a configured AI command.
type Harness struct {
	// argv is the command and its fixed arguments, before the prompt.
	argv []string
	// dir is the working directory. Empty inherits the process's own.
	dir string
	// timeout bounds one run.
	timeout time.Duration
	exec    Exec
}

// New builds a harness over a configured command line.
//
// command is split on whitespace with no quote handling, which is documented at
// the setting (config.AI.Command): an invocation needing more than that wants a
// wrapper script, which is one line and legible from the outside — unlike a
// quoting dialect invented here.
//
// An empty command yields a nil harness rather than an error. "Not configured"
// is the ordinary state of this feature, and every caller already has to render
// differently for it.
func New(command, dir string, timeout time.Duration) *Harness {
	argv := strings.Fields(strings.TrimSpace(command))
	if len(argv) == 0 {
		return nil
	}
	if timeout <= 0 {
		timeout = time.Minute
	}
	return &Harness{argv: argv, dir: strings.TrimSpace(dir), timeout: timeout, exec: execCommand}
}

// WithExec overrides the process seam; intended for tests.
func (h *Harness) WithExec(e Exec) *Harness {
	h.exec = e
	return h
}

// Program is the harness binary, as configured. It is reported by `riggs
// capabilities`, which probes PATH for it.
func (h *Harness) Program() string {
	if h == nil || len(h.argv) == 0 {
		return ""
	}
	return h.argv[0]
}

// Dir is the configured working directory, or empty for the process's own.
func (h *Harness) Dir() string {
	if h == nil {
		return ""
	}
	return h.dir
}

// promptFlags maps a known harness onto the flag that introduces its prompt.
//
// The list is short on purpose, and Claude Code is the only entry: it is the
// harness on the machine this runs on, and guessing at another tool's calling
// convention from its name is how a scheduled job ends up passing a review
// prompt to something that reads it as a filename.
//
// Anything not listed gets the prompt as its first argument — which is both the
// documented contract for a custom command and what the overwhelming majority
// of one-shot CLIs do.
var promptFlags = map[string][]string{
	"claude": {"-p"},
}

// Argv renders the full command line for one prompt.
//
// The program is matched on its BASE NAME, so `/opt/homebrew/bin/claude` and a
// bare `claude` are the same harness. Fixed arguments from the configured
// command are kept, and the prompt flag goes immediately before the prompt —
// after them, because a flag's value has to be adjacent to it and the admin's
// own arguments are not ours to interleave.
func (h *Harness) Argv(prompt string) []string {
	if h == nil || len(h.argv) == 0 {
		return nil
	}
	out := append([]string{}, h.argv...)
	out = append(out, promptFlags[filepath.Base(h.argv[0])]...)
	return append(out, prompt)
}

// Result is what one run produced.
type Result struct {
	// Output is the harness's combined stdout and stderr, verbatim.
	Output string
	// Duration is how long it took, for the line that reports it.
	Duration time.Duration
	// TimedOut reports that the bound was reached rather than the harness
	// having failed on its own terms. The two need different words: one is a
	// review that went wrong, the other is a review that may well still have
	// been going.
	TimedOut bool
}

// Run executes prompt and waits for the harness to finish.
//
// The timeout is applied here rather than left to the caller's context, because
// the caller's context is the daemon's and lives as long as the process. An
// unbounded run under a daemon nobody is watching is indistinguishable from a
// hung one right up until the machine runs out of them.
func (h *Harness) Run(ctx context.Context, prompt string) (Result, error) {
	if h == nil {
		return Result{}, fmt.Errorf("no AI command is configured (set ai.command)")
	}
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	started := time.Now()
	out, err := h.exec(ctx, h.dir, h.Argv(prompt))
	result := Result{
		Output:   strings.TrimSpace(string(out)),
		Duration: time.Since(started),
		TimedOut: ctx.Err() == context.DeadlineExceeded,
	}
	if result.TimedOut {
		return result, fmt.Errorf("%s gave up after %s", h.Program(), h.timeout)
	}
	if err != nil {
		return result, fmt.Errorf("%s failed: %w", h.Program(), err)
	}
	return result, nil
}

// execCommand is the live process seam.
func execCommand(ctx context.Context, dir string, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("ai: nothing to run")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.Bytes(), err
}
