package ai

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/slack"
)

// Running one item through the harness, and saying so where it was asked for.
//
// The shape is deliberately not internal/ask's. An ask posts a CARD about the
// item somewhere else entirely — another channel, or a DM to the person being
// asked — because its whole point is to reach somebody who is not looking at
// the digest. A run reaches nobody: the work happens on this machine and its
// result lands wherever the prompt sent it. So it says what it is doing in the
// thread of the message that was clicked, and nowhere else.
//
// One line, updated in place, rather than one message per state. A run takes
// minutes; three messages saying "started", "still going", "finished" would
// bury the digest under its own progress report.

// Poster is the Slack seam this needs: post a line, then rewrite it.
//
// Narrower than slack.Poster, which also deletes messages and inspects threads.
// Neither belongs to a harness run, and a fake in this package's tests should
// not have to implement them to prove that a failing command says so.
type Poster interface {
	Post(ctx context.Context, target slack.Target, msg slack.Message) (slack.Ref, error)
	Update(ctx context.Context, target slack.Target, ref slack.Ref, msg slack.Message) error
}

// Item is what one run is about: a pull request, or a ticket.
type Item struct {
	// Ref is the item's identity, as the reader knows it — `owner/repo#7`, or
	// `NYX-123`. It is what the prompt's `{ref}`/`{key}` becomes and what the
	// status line names.
	Ref string
	// URL is its browser link, which the prompt's `{url}` becomes.
	URL string
}

// outputTail bounds how much of a failed run's output is quoted back.
//
// Twelve lines and 1200 characters: enough to carry a stack trace's top or a
// "command not found", short enough that a harness which failed by printing its
// entire help text does not push the digest off the screen. The rest is on the
// machine, where somebody debugging this is going to end up anyway.
const (
	outputTailLines = 12
	outputTailChars = 1200
)

// Runner runs one domain's items through a harness and narrates the outcome.
//
// It is long-lived, unlike the click handlers around it. Those build a ledger
// and a GitHub client per click and close them again, because both go stale
// between the handful of clicks a week anyone makes; this holds a command line,
// a timeout and a map of what is currently running, none of which can go stale
// and one of which is the entire point of holding it.
type Runner struct {
	harness *Harness
	poster  Poster
	// prompt is the configured wording, read per run rather than captured, so
	// an edit on the Home tab takes effect on the next click rather than the
	// next restart (§7e).
	prompt func() string
	// label names the work in the status line: "code review", "AI assistance".
	label string

	// mu guards inflight, which is the set of items currently running.
	mu       sync.Mutex
	inflight map[string]bool
}

// NewRunner builds a runner over a harness. A nil harness is the unconfigured
// state, and Run reports it rather than pretending.
func NewRunner(h *Harness, poster Poster, label string, prompt func() string) *Runner {
	return &Runner{harness: h, poster: poster, label: label, prompt: prompt,
		inflight: map[string]bool{}}
}

// Enabled reports whether a harness is configured. Callers use it to decide
// whether to render the option at all: a control that cannot act is worse than
// one that was never there.
func (r *Runner) Enabled() bool { return r != nil && r.harness != nil }

// Run starts the harness for item and reports the outcome in thread.
//
// It BLOCKS for the length of the run. That is correct here: the daemon has
// already acknowledged the click and dispatched it on its own goroutine, so the
// only thing waiting is the goroutine whose job this is.
//
// target supplies the credentials and the conversation; threadTS is the message
// the click came from. With no thread — a run started from somewhere that is
// not a message — the narration is skipped rather than dropped at the bottom of
// a channel, and the outcome comes back as a return value instead.
func (r *Runner) Run(ctx context.Context, item Item, target slack.Target, threadTS string) (Result, error) {
	if !r.Enabled() {
		return Result{}, fmt.Errorf("no AI command is configured, so %s cannot be run here (set ai.command)", r.label)
	}
	if item.Ref == "" {
		return Result{}, fmt.Errorf("no item to run %s on", r.label)
	}
	if err := r.claim(item.Ref); err != nil {
		return Result{}, err
	}
	defer r.release(item.Ref)

	// Posted before the harness starts, not after. A run takes minutes, and a
	// menu option that shows nothing for four of them reads as one that did not
	// work — which is exactly the complaint that put a failure reporter in the
	// daemon.
	ref := r.say(ctx, target, threadTS, slack.Ref{}, fmt.Sprintf(
		"%s Running %s on %s. This takes a few minutes.", blockkit.MarkerRunning, r.label, item.Ref))

	result, runErr := r.harness.Run(ctx, Text(r.prompt(), item.Ref, item.URL))
	r.say(ctx, target, threadTS, ref, r.outcome(item, result, runErr))
	if runErr != nil {
		// Marked: the line above has already put this in front of the person who
		// clicked, and the daemon would otherwise report the same failure again
		// in a second message.
		return result, slack.Reported(runErr)
	}
	return result, nil
}

// outcome is the line a finished run leaves behind.
func (r *Runner) outcome(item Item, result Result, err error) string {
	took := result.Duration.Round(time.Second)
	if err == nil {
		return fmt.Sprintf("%s Finished %s on %s in %s.", blockkit.MarkerDone, r.label, item.Ref, took)
	}
	line := fmt.Sprintf("%s Could not finish %s on %s after %s — %v",
		blockkit.MarkerFailed, r.label, item.Ref, took, err)
	if tail := tail(result.Output); tail != "" {
		line += "\n```\n" + tail + "\n```"
	}
	return line
}

// say posts the status line, or rewrites the one already there.
//
// Every failure here is swallowed and the zero Ref returned. Slack declining to
// carry the commentary is not a reason to abandon the run, and on the closing
// call there is nothing left to report it to — the run is over either way, and
// its result is on the pull request rather than in this message.
func (r *Runner) say(ctx context.Context, target slack.Target, threadTS string, existing slack.Ref, text string) slack.Ref {
	if r.poster == nil || threadTS == "" {
		return slack.Ref{}
	}
	msg := slack.Message{Text: text, Blocks: blockkit.ContextBlocks(text), ThreadTS: threadTS}
	if existing.TS != "" {
		if err := r.poster.Update(ctx, target, existing, msg); err == nil {
			return existing
		}
		// The line it would have rewritten is gone — deleted, or posted by an
		// app whose token no longer works. A fresh message is the honest
		// fallback: the outcome matters more than where it sits.
	}
	ref, err := r.poster.Post(ctx, target, msg)
	if err != nil {
		return slack.Ref{}
	}
	return ref
}

// claim reserves an item, refusing a second concurrent run of the same one.
//
// Per item, not per machine. Two harnesses reviewing two different pull
// requests is what a busy morning looks like and is nobody's problem; two
// reviewing the SAME one is a double-click, and the second is pure waste that
// would also race the first to comment.
//
// There is deliberately no global cap. These are started by hand, one click at
// a time, and a limit that silently refused the second is a worse failure than
// two processes on a machine that can afford them.
func (r *Runner) claim(ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight[ref] {
		return fmt.Errorf("%s is already running on %s", r.label, ref)
	}
	r.inflight[ref] = true
	return nil
}

// release drops the claim.
func (r *Runner) release(ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inflight, ref)
}

// tail is the last few lines of a failed run's output, cut to what a Slack
// block will carry.
//
// The END rather than the beginning: a harness that failed says why last, after
// however much progress it narrated first.
func tail(output string) string {
	out := strings.TrimSpace(output)
	if out == "" {
		return ""
	}
	lines := strings.Split(out, "\n")
	if len(lines) > outputTailLines {
		lines = lines[len(lines)-outputTailLines:]
	}
	out = strings.Join(lines, "\n")
	if runes := []rune(out); len(runes) > outputTailChars {
		out = "…" + string(runes[len(runes)-outputTailChars:])
	}
	// Backticks would close the fence this is about to be wrapped in and spill
	// the rest of the output into the message as markup.
	return strings.ReplaceAll(out, "```", "'''")
}
