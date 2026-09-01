package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miere/riggs-mcp/internal/slack"
)

// poster records what the runner narrated, in order.
type poster struct {
	mu      sync.Mutex
	posts   []slack.Message
	updates []slack.Message
	postErr error
	updErr  error
	ts      int
}

func (p *poster) Post(_ context.Context, _ slack.Target, msg slack.Message) (slack.Ref, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.posts = append(p.posts, msg)
	if p.postErr != nil {
		return slack.Ref{}, p.postErr
	}
	p.ts++
	return slack.Ref{Channel: "C-digest", TS: fmt.Sprintf("170%d.1", p.ts)}, nil
}

func (p *poster) Update(_ context.Context, _ slack.Target, _ slack.Ref, msg slack.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.updates = append(p.updates, msg)
	return p.updErr
}

func runner(t *testing.T, rec *recorder, p *poster) *Runner {
	t.Helper()
	return NewRunner(harness(t, "claude", rec), p, "a code review",
		func() string { return "Review {ref} at {url}." })
}

var digest = slack.Target{Profile: "riggs", BotToken: "xoxb", Channel: "C-digest"}

// A run takes minutes. A menu option that shows nothing for four of them reads
// as one that did not work, which is the complaint that put a failure reporter
// in the daemon in the first place.
func TestRunSaysSoBeforeItStarts(t *testing.T) {
	rec, p := &recorder{out: "reviewed"}, &poster{}
	item := Item{Ref: "o/r#7", URL: "https://github.com/o/r/pull/7"}

	if _, err := runner(t, rec, p).Run(context.Background(), item, digest, "1700.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(p.posts) != 1 {
		t.Fatalf("posts = %d, want the one status line", len(p.posts))
	}
	if !strings.Contains(p.posts[0].Text, "Running a code review on o/r#7") {
		t.Fatalf("opening line = %q", p.posts[0].Text)
	}
	if p.posts[0].ThreadTS != "1700.1" {
		t.Fatalf("the line was not threaded under the clicked message: %+v", p.posts[0])
	}
	// One line, updated in place. Three messages saying "started", "still
	// going", "finished" would bury the digest under its own progress report.
	if len(p.updates) != 1 {
		t.Fatalf("updates = %d, want the outcome rewritten in place", len(p.updates))
	}
	if !strings.Contains(p.updates[0].Text, "Finished a code review on o/r#7") {
		t.Fatalf("closing line = %q", p.updates[0].Text)
	}
}

// The prompt reaches the harness with its subject in it, read at run time so a
// reworded prompt takes effect on the next click rather than the next restart.
func TestRunHandsTheHarnessTheRenderedPrompt(t *testing.T) {
	rec, p := &recorder{}, &poster{}
	item := Item{Ref: "o/r#7", URL: "https://github.com/o/r/pull/7"}

	if _, err := runner(t, rec, p).Run(context.Background(), item, digest, "1700.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := rec.lastPrompt()
	if got != "Review o/r#7 at https://github.com/o/r/pull/7." {
		t.Fatalf("prompt = %q", got)
	}
}

func TestRunReadsThePromptAtEachRun(t *testing.T) {
	rec, p := &recorder{}, &poster{}
	prompt := "first {ref}"
	r := NewRunner(harness(t, "claude", rec), p, "a code review", func() string { return prompt })
	item := Item{Ref: "o/r#7", URL: "u"}

	if _, err := r.Run(context.Background(), item, digest, "1700.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	prompt = "second {ref}"
	if _, err := r.Run(context.Background(), item, digest, "1700.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := rec.lastPrompt(); !strings.HasPrefix(got, "second") {
		t.Fatalf("prompt = %q, want the reworded one", got)
	}
}

// A failure carries the tail of what the harness said, because that is the
// whole value of the line.
func TestAFailedRunQuotesTheTail(t *testing.T) {
	rec := &recorder{out: "step one\nstep two\nfatal: no such repository", err: errors.New("exit status 128")}
	p := &poster{}

	_, err := runner(t, rec, p).Run(context.Background(), Item{Ref: "o/r#7", URL: "u"}, digest, "1700.1")
	if err == nil {
		t.Fatal("a failing run reported success")
	}
	// Marked as reported: the line below has already shown it to the person who
	// clicked, and the daemon would otherwise say the same thing again.
	if !slack.WasReported(err) {
		t.Fatalf("the failure was not marked as reported: %v", err)
	}
	text := p.updates[0].Text
	if !strings.Contains(text, "Could not finish a code review on o/r#7") {
		t.Fatalf("closing line = %q", text)
	}
	if !strings.Contains(text, "fatal: no such repository") {
		t.Fatalf("the output tail was dropped: %q", text)
	}
}

// A harness that failed by printing its entire help text must not push the
// digest off the screen.
func TestTheTailIsBounded(t *testing.T) {
	var lines []string
	for i := 0; i < 60; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	got := tail(strings.Join(lines, "\n"))
	if n := strings.Count(got, "\n") + 1; n > outputTailLines {
		t.Fatalf("tail kept %d lines, want at most %d", n, outputTailLines)
	}
	// The END, not the beginning: a harness that failed says why last.
	if !strings.Contains(got, "line 59") {
		t.Fatalf("tail dropped the last line: %q", got)
	}
	// Backticks would close the fence the tail is wrapped in and spill the rest
	// of the output into the message as markup.
	if strings.Contains(tail("a ``` b"), "```") {
		t.Fatal("a fence in the output survived into the message")
	}
}

// A double-click is the case this exists for: the second run is pure waste and
// would also race the first to comment.
func TestTheSameItemCannotRunTwiceAtOnce(t *testing.T) {
	rec := &recorder{block: 50 * time.Millisecond}
	p := &poster{}
	r := runner(t, rec, p)
	item := Item{Ref: "o/r#7", URL: "u"}

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := r.Run(context.Background(), item, digest, "1700.1")
		done <- err
	}()
	<-started

	var second error
	for i := 0; i < 100; i++ {
		if _, second = r.Run(context.Background(), item, digest, "1700.1"); second != nil {
			break
		}
	}
	if second == nil || !strings.Contains(second.Error(), "already running") {
		t.Fatalf("second run = %v, want it refused", second)
	}
	if err := <-done; err != nil {
		t.Fatalf("first run: %v", err)
	}
	// And the claim is released, so the next click works.
	if _, err := r.Run(context.Background(), item, digest, "1700.1"); err != nil {
		t.Fatalf("a later run was still blocked: %v", err)
	}
}

// Different items are a busy morning, not a problem. There is deliberately no
// global cap: these are started by hand, one click at a time.
func TestDifferentItemsRunConcurrently(t *testing.T) {
	rec := &recorder{block: 20 * time.Millisecond}
	r := runner(t, rec, &poster{})

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, ref := range []string{"o/r#7", "o/r#8"} {
		wg.Add(1)
		go func(i int, ref string) {
			defer wg.Done()
			_, errs[i] = r.Run(context.Background(), Item{Ref: ref, URL: "u"}, digest, "1700.1")
		}(i, ref)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}

// Slack declining to carry the commentary is not a reason to abandon the run.
func TestSlackFailuresDoNotStopTheRun(t *testing.T) {
	rec := &recorder{out: "reviewed"}
	p := &poster{postErr: errors.New("channel_not_found")}

	if _, err := runner(t, rec, p).Run(context.Background(), Item{Ref: "o/r#7", URL: "u"}, digest, "1700.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.lastPrompt() == "" {
		t.Fatal("the harness never ran")
	}
}

// With no thread there is nowhere to narrate, and dropping a status line at the
// bottom of a channel is worse than saying nothing.
func TestWithNoThreadNothingIsPosted(t *testing.T) {
	rec, p := &recorder{}, &poster{}
	if _, err := runner(t, rec, p).Run(context.Background(), Item{Ref: "o/r#7", URL: "u"}, digest, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.posts)+len(p.updates) != 0 {
		t.Fatalf("posted %d, updated %d, want silence", len(p.posts), len(p.updates))
	}
}

// An unconfigured harness names the setting, on the same rule `riggs
// capabilities` follows: never leave "why is this missing?" to the source.
func TestAnUnconfiguredRunnerNamesTheSetting(t *testing.T) {
	r := NewRunner(nil, &poster{}, "a code review", func() string { return "x" })
	if r.Enabled() {
		t.Fatal("Enabled with no harness")
	}
	_, err := r.Run(context.Background(), Item{Ref: "o/r#7"}, digest, "1700.1")
	if err == nil || !strings.Contains(err.Error(), "ai.command") {
		t.Fatalf("err = %v, want it to name the setting", err)
	}
}
