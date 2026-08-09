// Package ai produces the one-paragraph card summaries, by shelling out to
// `claude -p`.
//
// It holds no credential: the Claude CLI carries its own auth, exactly as `gh`
// does for GitHub. That keeps Riggs out of the business of storing another
// key, and means a machine that can already run Claude Code needs nothing
// extra.
package ai

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Runner executes the summariser binary. It is the seam that keeps `claude`
// out of the call sites, so the reconcile loop is fakeable end to end.
type Runner func(ctx context.Context, name string, args ...string) (stdout []byte, err error)

// Summariser turns a title and description into a short prose summary.
type Summariser interface {
	// Summarise returns the summary, and an error describing any degradation.
	// The returned string is ALWAYS usable: on failure it is the title, so a
	// summary hiccup can never block a card and therefore a review.
	Summarise(ctx context.Context, title, body string) (string, error)
}

// Claude summarises via the Claude Code CLI.
type Claude struct {
	run     Runner
	bin     string
	timeout time.Duration
}

// NewClaude builds a summariser over the `claude` binary, resolved from
// $CLAUDE_BIN, then PATH, then the conventional install location.
func NewClaude() *Claude {
	return &Claude{run: execRunner, bin: claudeBin(), timeout: 2 * time.Minute}
}

// WithRunner overrides the process seam; intended for tests.
func (c *Claude) WithRunner(r Runner) *Claude {
	c.run = r
	return c
}

func claudeBin() string {
	if b := os.Getenv("CLAUDE_BIN"); b != "" {
		return b
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "claude"
	}
	return filepath.Join(home, ".local", "bin", "claude")
}

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// prompt is the instruction, kept identical to the Python's so migrated cards
// read the same.
const prompt = "Summarise this GitHub pull request in one short paragraph, at most 4 " +
	"sentences, plain prose — no preamble, no markdown, no bullet points.\n\n" +
	"Title: %s\n\nDescription:\n%s"

// Summarise asks Claude for a one-paragraph summary, degrading to the title.
func (c *Claude) Summarise(ctx context.Context, title, body string) (string, error) {
	if body == "" {
		body = "(no description)"
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	out, err := c.run(ctx, c.bin, "-p", fmt.Sprintf(prompt, title, body))
	if err != nil {
		return title, fmt.Errorf("claude summary failed: %w", err)
	}
	summary := strings.TrimSpace(string(out))
	if summary == "" {
		return title, fmt.Errorf("claude returned an empty summary")
	}
	return summary, nil
}

// Titles is a Summariser that never calls anything: it returns the title. It
// backs `--no-summary`, and is what runs when the claude binary is absent.
type Titles struct{}

// Summarise returns the title unchanged.
func (Titles) Summarise(_ context.Context, title, _ string) (string, error) { return title, nil }
