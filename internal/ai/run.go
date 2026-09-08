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
// result lands wherever the prompt sent it. So when something goes wrong it says
// so in the thread of the message that was clicked, and nowhere else.
//
// It says nothing at all when nothing goes wrong. A run takes minutes and used
// to hold its place with a "Running…" line rewritten at the end; the
// acknowledgement reaction on the clicked message now covers that gap for free
// (§7f), which leaves this with only the half a reaction cannot carry — what
// failed, and what the harness printed on its way out.

// Poster is the Slack seam this needs: post a line.
//
// Narrower than slack.Poster, which also updates, deletes and inspects threads.
// None of those belongs to a harness run any more — the placeholder that needed
// Update is gone — and a fake in this package's tests should not have to
// implement them to prove that a failing command says so.
type Poster interface {
	Post(ctx context.Context, target slack.Target, msg slack.Message) (slack.Ref, error)
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

	// Nothing is said before the run any more, and nothing after a successful
	// one.
	//
	// A run takes minutes, and the "Running…, this takes a few minutes" line
	// existed because an option that shows nothing for four of them reads as
	// one that did not work. That reasoning is intact; what changed is the
	// cheaper way of saying it. The acknowledgement reaction goes on the digest
	// before this is called and stays there for the whole run, so the wait is
	// visibly covered without a message — and a message that was going to be
	// rewritten a few minutes later was always the expensive way to hold a
	// place.
	//
	// A failure still gets one, because it carries something a reaction cannot:
	// which item, how long, and the tail of the harness's own output.
	result, runErr := r.harness.Run(ctx, Text(r.prompt(), item.Ref, item.URL))
	if runErr != nil {
		r.say(ctx, target, threadTS, r.failure(item, result, runErr))
		// Marked: the line above has already put this in front of the person who
		// clicked, and the daemon would otherwise report the same failure again
		// in a second message.
		return result, slack.Reported(runErr)
	}
	return result, nil
}

// failure is the line a run that did not finish leaves behind.
//
// The tail of the harness's output is on it because that is the whole reason a
// failure is still a message: "it went wrong" is what the warning reaction
// already says, and the only thing worth a notification is the part that says
// what went wrong.
func (r *Runner) failure(item Item, result Result, err error) string {
	line := fmt.Sprintf("%s Could not finish %s on %s after %s — %v",
		blockkit.MarkerFailed, r.label, item.Ref, result.Duration.Round(time.Second), err)
	if tail := tail(result.Output); tail != "" {
		line += "\n```\n" + tail + "\n```"
	}
	return line
}

// say posts one line into the thread the run was started from.
//
// It used to post a placeholder and then rewrite it, which is why it took an
// existing Ref and returned one. With only failures left to report there is
// never a line already there to rewrite, so the update branch and both refs are
// gone — a post-or-update helper with exactly one caller that always posts is a
// second code path nothing exercises.
//
// A failure here is swallowed. Slack declining to carry the commentary is not a
// reason to change what the run reports: the harness has already finished, and
// the daemon's own failure reporter is still behind this.
func (r *Runner) say(ctx context.Context, target slack.Target, threadTS, text string) {
	if r.poster == nil || threadTS == "" {
		return
	}
	_, _ = r.poster.Post(ctx, target, slack.Message{
		Text: text, Blocks: blockkit.ContextBlocks(text), ThreadTS: threadTS,
	})
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
