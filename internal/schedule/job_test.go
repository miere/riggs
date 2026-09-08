package schedule

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// There are two front doors — the Home tab's modal and `riggs jobs add` — and a
// rule enforced in only one of them is a rule that is not enforced.
func TestNewJobValidates(t *testing.T) {
	args := []string{"git", "pr", "--bulk", "miere"}

	for name, tc := range map[string]struct {
		job     string
		args    []string
		spec    string
		timeout time.Duration
		want    string
	}{
		"no name":                {"", args, "3m", 0, "needs a name"},
		"a name with a space":    {"my job", args, "3m", 0, "not a usable job name"},
		"a name with a slash":    {"a/b", args, "3m", 0, "not a usable job name"},
		"nothing to run":         {"digest", nil, "3m", 0, "nothing to run"},
		"an unreadable schedule": {"digest", args, "weekly", 0, "neither a duration"},
		"a negative timeout":     {"digest", args, "3m", -time.Second, "cannot be negative"},
		"an absurd timeout":      {"digest", args, "3m", 2 * time.Hour, "is a service, not a job"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewJob(tc.job, tc.args, tc.spec, tc.timeout, true)
			if err == nil {
				t.Fatalf("NewJob accepted %+v", tc)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	job, err := NewJob("github-review-queue", args, " 3m ", 0, true)
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if job.Timeout != DefaultTimeout {
		t.Fatalf("timeout = %v, want the default", job.Timeout)
	}
	if job.Spec != "3m" {
		t.Fatalf("spec = %q, want it trimmed", job.Spec)
	}
}

// The binary is not the operator's to choose — every job runs THIS build, at
// the path this daemon was started from — and typing the whole command Murtaugh
// used to run is the obvious thing to do.
func TestSplitArgsDropsALeadingRiggs(t *testing.T) {
	for _, command := range []string{
		"riggs git pr --bulk miere",
		"git pr --bulk miere",
		"  RIGGS   git pr --bulk miere  ",
	} {
		got, err := SplitArgs(command)
		if err != nil {
			t.Fatalf("SplitArgs(%q): %v", command, err)
		}
		if strings.Join(got, " ") != "git pr --bulk miere" {
			t.Fatalf("SplitArgs(%q) = %v", command, got)
		}
	}
	if got, err := SplitArgs("   "); err != nil || len(got) != 0 {
		t.Fatalf("SplitArgs(blank) = %v, %v", got, err)
	}
}

// The whole point of the exercise: a JQL that works in the Jira UI works here,
// as one argument, with its own quoting intact.
func TestSplitArgsKeepsAQuotedJQLWhole(t *testing.T) {
	const jql = `project = NYX AND labels = "ai-able" AND assignee IS EMPTY AND status = "Ready" AND sprint IN openSprints()`

	got, err := SplitArgs(`jira tickets --bulk '` + jql + `' --slack-channel C0B29C20Z9S`)
	if err != nil {
		t.Fatalf("SplitArgs: %v", err)
	}
	want := []string{"jira", "tickets", "--bulk", jql, "--slack-channel", "C0B29C20Z9S"}
	if !slices.Equal(got, want) {
		t.Fatalf("SplitArgs = %q, want %q", got, want)
	}
}

// The quoting dialect, one rule per case. It is deliberately small: a job is
// argv handed to exec, never a line handed to a shell, so nothing here expands.
func TestSplitArgsQuoting(t *testing.T) {
	for name, tc := range map[string]struct {
		command string
		want    []string
	}{
		"double quotes group":         {`a "b c" d`, []string{"a", "b c", "d"}},
		"single quotes are literal":   {`a 'b "c" \d' e`, []string{"a", `b "c" \d`, "e"}},
		"escaped quote inside":        {`"say \"hi\""`, []string{`say "hi"`}},
		"escaped backslash inside":    {`"a\\b"`, []string{`a\b`}},
		"other backslashes survive":   {`"\d+"`, []string{`\d+`}},
		"a bare escape":               {`a\ b`, []string{"a b"}},
		"quotes join their neighbour": {`--bulk="a b"`, []string{`--bulk=a b`}},
		"an empty argument":           {`a '' b`, []string{"a", "", "b"}},
		"nothing is expanded":         {`$HOME *.go # x`, []string{"$HOME", "*.go", "#", "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := SplitArgs(tc.command)
			if err != nil {
				t.Fatalf("SplitArgs(%q): %v", tc.command, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("SplitArgs(%q) = %q, want %q", tc.command, got, tc.want)
			}
		})
	}
}

// An unterminated quote is refused, not guessed at. The guess is always "the
// operator meant the rest of the line", and it is wrong half the time.
func TestSplitArgsRefusesAnUnfinishedCommand(t *testing.T) {
	for name, tc := range map[string]struct{ command, want string }{
		"an open single quote": {`--bulk 'project = NYX`, `unterminated ' quote`},
		"an open double quote": {`--bulk "project = NYX`, `unterminated " quote`},
		"a trailing backslash": {`--bulk a\`, "ends in a backslash"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := SplitArgs(tc.command)
			if err == nil {
				t.Fatalf("SplitArgs(%q) was accepted", tc.command)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// `riggs jobs add` has a shell upstream that already worked the quoting out, so
// nothing re-splits its argv. All TrimBinary still does is forgive the operator
// for typing the binary's name.
func TestTrimBinary(t *testing.T) {
	argv := []string{"jira", "tickets", "--bulk", `project = NYX AND status = "Ready"`}

	if got := TrimBinary(argv); !slices.Equal(got, argv) {
		t.Fatalf("TrimBinary = %q, want it untouched", got)
	}
	if got := TrimBinary(append([]string{"RIGGS"}, argv...)); !slices.Equal(got, argv) {
		t.Fatalf("TrimBinary = %q, want the binary dropped", got)
	}
	if got := TrimBinary(nil); len(got) != 0 {
		t.Fatalf("TrimBinary(nil) = %q", got)
	}
}

// A round trip through the modal: what it shows is what somebody typed.
func TestCommandRendersTheArguments(t *testing.T) {
	argv, err := SplitArgs("riggs jira tickets --bulk")
	if err != nil {
		t.Fatalf("SplitArgs: %v", err)
	}
	job, err := NewJob("digest", argv, "3m", 0, true)
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if got := Command(job); got != "jira tickets --bulk" {
		t.Fatalf("Command = %q", got)
	}
}

// The property that stops a correct job coming apart when somebody opens it to
// change the schedule: Command renders what the edit modal is prefilled with,
// and SplitArgs reads that back. Anything lost between the two is lost silently.
func TestCommandRoundTripsThroughSplitArgs(t *testing.T) {
	for name, argv := range map[string][]string{
		"a JQL with double quotes": {"jira", "tickets", "--bulk",
			`project = NYX AND labels = "ai-able" AND status = "Ready" AND sprint IN openSprints()`},
		"a value with a single quote": {"jira", "tickets", "--bulk", `summary ~ "o'brien"`},
		"a backslash":                 {"jira", "tickets", "--bulk", `summary ~ "\\d+"`},
		"an empty argument":           {"git", "pr", "--bulk", ""},
		"nothing needing quotes":      {"git", "pr", "--bulk", "miere"},
	} {
		t.Run(name, func(t *testing.T) {
			rendered := Command(Job{Args: argv})
			got, err := SplitArgs(rendered)
			if err != nil {
				t.Fatalf("SplitArgs(%q): %v", rendered, err)
			}
			if !slices.Equal(got, argv) {
				t.Fatalf("round trip of %q via %q = %q", argv, rendered, got)
			}
		})
	}
}
