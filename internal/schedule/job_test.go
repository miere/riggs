package schedule

import (
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
		got := SplitArgs(command)
		if strings.Join(got, " ") != "git pr --bulk miere" {
			t.Fatalf("SplitArgs(%q) = %v", command, got)
		}
	}
	if got := SplitArgs("   "); len(got) != 0 {
		t.Fatalf("SplitArgs(blank) = %v", got)
	}
}

// A round trip through the modal: what it shows is what somebody typed.
func TestCommandRendersTheArguments(t *testing.T) {
	job, err := NewJob("digest", SplitArgs("riggs jira tickets --bulk"), "3m", 0, true)
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if got := Command(job); got != "jira tickets --bulk" {
		t.Fatalf("Command = %q", got)
	}
}
