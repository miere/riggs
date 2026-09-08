package schedule

import (
	"slices"
	"strings"
	"testing"
)

// There are two front doors — the Home tab's modals and `riggs jobs add` — and a
// rule enforced in only one of them is a rule that is not enforced.
func TestNewJobValidates(t *testing.T) {
	for name, tc := range map[string]struct {
		job   string
		value string
		spec  string
		jira  bool
		want  string
	}{
		"no name":                 {"", "miere", "3m", false, "needs a name"},
		"a name with a space":     {"my job", "miere", "3m", false, "not a usable job name"},
		"a name with a slash":     {"a/b", "miere", "3m", false, "not a usable job name"},
		"no login":                {"digest", "  ", "3m", false, "GitHub username is required"},
		"a login with a space":    {"digest", "two names", "3m", false, "has a space in it"},
		"no query":                {"digest", "", "3m", true, "JQL query is required"},
		"an unreadable schedule":  {"digest", "miere", "weekly", false, "neither a duration"},
		"an unreadable JQL spec":  {"digest", "project = NYX", "weekly", true, "neither a duration"},
		"a schedule that is gone": {"digest", "miere", "", false, "schedule is required"},
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			if tc.jira {
				_, err = NewJiraJob(tc.job, tc.value, tc.spec)
			} else {
				_, err = NewGitHubJob(tc.job, tc.value, tc.spec)
			}
			if err == nil {
				t.Fatalf("the constructor accepted %+v", tc)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A new job is enabled, typed, and carries exactly the parameter its kind
// declares — which is what the ledger stores and what Args reads back.
func TestNewGitHubJob(t *testing.T) {
	job, err := NewGitHubJob(" github-review-queue ", " miere ", " 3m ")
	if err != nil {
		t.Fatalf("NewGitHubJob: %v", err)
	}
	if job.Name != "github-review-queue" || job.Spec != "3m" {
		t.Fatalf("job = %+v, want its name and spec trimmed", job)
	}
	if KindOf(job) != KindGitHubReviews {
		t.Fatalf("kind = %q", job.Type)
	}
	if got := Param(job, ParamLogin); got != "miere" {
		t.Fatalf("login = %q, want it trimmed", got)
	}
	if !job.Enabled {
		t.Fatal("a new job is created disabled")
	}
}

// The whole point of the exercise: a JQL that works in the Jira UI is stored as
// ONE parameter, with its own quoting intact, and never goes near a splitter.
func TestNewJiraJobKeepsTheQueryWhole(t *testing.T) {
	const jql = `project = NYX AND labels = "ai-able" AND assignee IS EMPTY AND status = "Ready" AND sprint IN openSprints()`

	job, err := NewJiraJob("tickets", jql, "3m")
	if err != nil {
		t.Fatalf("NewJiraJob: %v", err)
	}
	if got := Param(job, ParamJQL); got != jql {
		t.Fatalf("jql = %q, want it verbatim", got)
	}
	args, err := Args(job)
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	want := []string{"jira", "tickets", "--bulk", jql}
	if !slices.Equal(args, want) {
		t.Fatalf("Args = %q, want %q", args, want)
	}
}

// The command spellings are a contract with internal/frontends/cli: the child
// process resolves them by exact match, so a rename here does not fail to build
// — it fails at 3am, in a job, with "unknown command".
func TestArgsSpellsTheCommandsTheCLIResolves(t *testing.T) {
	github, err := NewGitHubJob("reviews", "miere", "3m")
	if err != nil {
		t.Fatalf("NewGitHubJob: %v", err)
	}
	args, err := Args(github)
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	if want := []string{"git", "pr", "--bulk", "miere"}; !slices.Equal(args, want) {
		t.Fatalf("Args = %q, want %q", args, want)
	}
}

// A job Riggs cannot render arguments for is refused rather than run with
// whatever it happens to have. The scheduler turns this into a recorded
// failure, which is how the Home tab's row gets to say what is wrong.
func TestArgsRefusesWhatItCannotRun(t *testing.T) {
	for name, job := range map[string]Job{
		"a kind from a newer Riggs": {Name: "x", Type: "slack-digest"},
		"no kind at all":            {Name: "x"},
		"a login that went missing": {Name: "x", Type: string(KindGitHubReviews)},
		"a query that went missing": {Name: "x", Type: string(KindJiraTickets), Params: map[string]string{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Args(job); err == nil {
				t.Fatalf("Args accepted %+v", job)
			}
		})
	}
}

// The row is where an operator checks what a job actually runs, so the line has
// to be readable as one command — a JQL rendered bare reads as eleven arguments.
func TestCommandQuotesWhatNeedsQuoting(t *testing.T) {
	job, err := NewJiraJob("tickets", `project = NYX AND status = "Ready"`, "3m")
	if err != nil {
		t.Fatalf("NewJiraJob: %v", err)
	}
	want := `jira tickets --bulk 'project = NYX AND status = "Ready"'`
	if got := Command(job); got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}

	// A kind this build does not know renders empty. The row above it already
	// says the kind is unknown; a second copy of the same news helps nobody.
	if got := Command(Job{Name: "x", Type: "slack-digest"}); got != "" {
		t.Fatalf("Command of an unknown kind = %q, want empty", got)
	}
}

// Every kind a job may be stored as has a spec, and every spec names a
// parameter the constructors actually set. A kind missing from this table is
// one whose rows render with no label and whose timeout cannot be configured.
func TestEveryKindIsDescribed(t *testing.T) {
	github, err := NewGitHubJob("a", "miere", "3m")
	if err != nil {
		t.Fatalf("NewGitHubJob: %v", err)
	}
	jira, err := NewJiraJob("b", "project = NYX", "3m")
	if err != nil {
		t.Fatalf("NewJiraJob: %v", err)
	}

	for _, job := range []Job{github, jira} {
		spec, ok := LookupKind(KindOf(job))
		if !ok {
			t.Fatalf("kind %q is not in the table", job.Type)
		}
		if spec.Label == "" || spec.ParamLabel == "" {
			t.Fatalf("kind %q has no label to render: %+v", job.Type, spec)
		}
		if Param(job, spec.Param) == "" {
			t.Fatalf("kind %q declares parameter %q, which its constructor does not set",
				job.Type, spec.Param)
		}
	}
	if _, ok := LookupKind("slack-digest"); ok {
		t.Fatal("LookupKind invented a kind")
	}
}
