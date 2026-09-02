package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeCommand records the arguments a command was invoked with.
type fakeCommand struct {
	got    map[string]any
	result any
	err    error
}

func (f *fakeCommand) Invoke(_ context.Context, a map[string]any) (any, error) {
	f.got = a
	if f.result == nil {
		f.result = "ok"
	}
	return f.result, f.err
}

// run drives the frontend and returns what it wrote to stdout.
func run(t *testing.T, argv ...string) (string, *fakeCommand, *fakeCommand, error) {
	t.Helper()
	reviews, tickets := &fakeCommand{}, &fakeCommand{}
	var out, errs bytes.Buffer
	err := New(reviews, tickets).WithOutput(&out, &errs).Run(context.Background(), argv)
	return out.String(), reviews, tickets, err
}

// The spelling is a contract with the scheduler: stored jobs invoke exactly
// this argv, and nothing validates it before the process starts.
func TestTheReviewDigestKeepsItsSpelling(t *testing.T) {
	out, reviews, _, err := run(t, "git", "pr", "--bulk", "miere")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reviews.got["user"] != "miere" {
		t.Fatalf("args = %v, want the login bound to user", reviews.got)
	}
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestTheTicketDigestKeepsItsSpelling(t *testing.T) {
	jql := `project = NYX AND labels = "ai-able"`
	_, _, tickets, err := run(t, "jira", "tickets", "--bulk", jql)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tickets.got["query"] != jql {
		t.Fatalf("args = %v, want the JQL bound to query", tickets.got)
	}
}

// Every flag a stored job may carry, in both spellings.
func TestFlagsReachTheCommand(t *testing.T) {
	_, reviews, _, err := run(t, "git", "pr", "--bulk", "miere",
		"--slack-channel", "C123", "--slack-profile=riggs",
		"--max-items", "5", "--cooldown", "3h", "--dry-run")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for key, want := range map[string]any{
		"user": "miere", "slack_channel": "C123", "slack_profile": "riggs",
		"max_items": int64(5), "cooldown": "3h", "dry_run": true,
	} {
		if reviews.got[key] != want {
			t.Errorf("%s = %v (%T), want %v", key, reviews.got[key], reviews.got[key], want)
		}
	}
}

// --dry-run is valueless. Requiring `--dry-run true` would be a new spelling,
// and the old frontend did not ask for one.
func TestDryRunTakesNoValue(t *testing.T) {
	_, reviews, _, err := run(t, "git", "pr", "--bulk", "miere", "--dry-run", "--max-items", "2")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reviews.got["dry_run"] != true || reviews.got["max_items"] != int64(2) {
		t.Fatalf("args = %v", reviews.got)
	}
}

func TestMissingFlagValueIsRejected(t *testing.T) {
	if _, _, _, err := run(t, "git", "pr", "--bulk", "miere", "--slack-channel"); err == nil {
		t.Fatal("a flag with no value was accepted")
	}
}

func TestNonNumericMaxItemsIsRejected(t *testing.T) {
	_, _, _, err := run(t, "git", "pr", "--bulk", "miere", "--max-items", "lots")
	if err == nil || !strings.Contains(err.Error(), "not a number") {
		t.Fatalf("err = %v", err)
	}
}

func TestUnknownCommandIsUsage(t *testing.T) {
	_, _, _, err := run(t, "git", "pr", "--approve", "o/r#1")
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("err = %v, want ErrUsage", err)
	}
	if _, _, _, err := run(t); !errors.Is(err, ErrUsage) {
		t.Fatalf("no args: err = %v, want ErrUsage", err)
	}
	// The retired MCP mode is now just an unknown command.
	if _, _, _, err := run(t, "mcp"); !errors.Is(err, ErrUsage) {
		t.Fatalf("mcp: err = %v, want ErrUsage", err)
	}
}

// An unconfigured machine has no digests. The error names the diagnostic
// rather than reading as a missing feature.
func TestAnUnconfiguredCommandExplainsItself(t *testing.T) {
	var out, errs bytes.Buffer
	err := New(nil, nil).WithOutput(&out, &errs).
		Run(context.Background(), []string{"git", "pr", "--bulk", "miere"})
	if err == nil {
		t.Fatal("an unconfigured digest ran")
	}
	if !strings.Contains(err.Error(), "capabilities") {
		t.Fatalf("err = %v, want it to point at the diagnostic", err)
	}
}

// --json-output may appear anywhere, and is never passed on as a parameter.
func TestJSONOutput(t *testing.T) {
	reviews, tickets := &fakeCommand{result: map[string]string{"ref": "o/r#1"}}, &fakeCommand{}
	var out, errs bytes.Buffer
	err := New(reviews, tickets).WithOutput(&out, &errs).
		Run(context.Background(), []string{"git", "--json-output", "pr", "--bulk", "miere"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(out.String()) != `{"ref":"o/r#1"}` {
		t.Fatalf("stdout = %q", out.String())
	}
	if _, passed := reviews.got["json_output"]; passed {
		t.Error("--json-output reached the command as a parameter")
	}
}
