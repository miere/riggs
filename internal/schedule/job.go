package schedule

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/miere/riggs-mcp/internal/notify"
)

// Job is the record the scheduler works from. It is the ledger's row (§9c),
// used directly rather than copied into a parallel struct that would have to be
// kept in step with it.
type Job = notify.Job

// A job has a KIND, and the kind is the whole definition.
//
// It used to be a name and a command line. That was defensible while the job
// table was a like-for-like port of Murtaugh's cron — the argv was the thing
// being migrated, byte for byte — and it stopped being defensible the moment an
// admin had to configure one from Slack. The Home tab could offer only a free
// text box saying "arguments for riggs", into which the operator was expected
// to type `jira tickets --bulk 'project = NYX AND labels = "ai-able"'` from
// memory, correctly, quoting and all, in a single-line Slack input.
//
// There are two things Riggs schedules, they have three parameters between
// them, and every one of those parameters has a right answer that a form can
// ask for directly. So the form asks: a GitHub login, or a JQL query, and a
// cadence. The argument list is DERIVED from the answer, here, in the one place
// that knows the spelling — which is also what makes the CLI's contract
// (internal/frontends/cli) enforceable rather than a comment asking people to
// be careful.
//
// The cost is that Riggs can no longer schedule an arbitrary command, and that
// is not a loss being tolerated: it is the feature. A scheduler that runs
// whatever argv a Slack modal contained is one where a typo runs every three
// minutes forever and the only symptom is a red line on a tab nobody opened.
type Kind string

const (
	// KindGitHubReviews is the pull-request digest: `git pr --bulk <login>`.
	KindGitHubReviews Kind = "github-reviews"
	// KindJiraTickets is the ticket digest: `jira tickets --bulk <jql>`.
	KindJiraTickets Kind = "jira-tickets"
)

// The parameter names each kind declares. They are the keys of Job.Params and
// they are stored in the ledger, so they are constants rather than literals
// spelled out at each use: a typo in one of these is a job that loads, renders,
// and then runs with an empty query.
const (
	// ParamLogin is whose review queue a GitHub digest fetches.
	ParamLogin = "login"
	// ParamJQL is the query a ticket digest advertises the results of.
	ParamJQL = "jql"
)

// KindSpec describes one kind for the surfaces that have to render it.
//
// A table rather than a switch in each caller, for the reason config.Prompts is
// one: the Home tab, the Configuration modal and the CLI all need the same
// three facts about a kind, and three copies of them is three chances to
// disagree about what a job is called.
type KindSpec struct {
	// Kind is the stored token.
	Kind Kind
	// Label is what a human calls it: "Pull Requests — Reviewer".
	Label string
	// Param is the one parameter this kind takes beyond its schedule.
	Param string
	// ParamLabel names that parameter on a form.
	ParamLabel string
}

// kinds is the table. Order is rendering order, and it is deliberate: the
// GitHub digest is the one job every install has.
var kinds = []KindSpec{
	{
		Kind:       KindGitHubReviews,
		Label:      "Pull Requests — Reviewer",
		Param:      ParamLogin,
		ParamLabel: "GitHub username",
	},
	{
		Kind:       KindJiraTickets,
		Label:      "Jira tickets",
		Param:      ParamJQL,
		ParamLabel: "JQL",
	},
}

// Kinds lists every kind of job this build can run.
func Kinds() []KindSpec { return append([]KindSpec(nil), kinds...) }

// LookupKind finds a kind's spec, and reports false for one this build does not
// know — a row written by a newer Riggs, or a hand-edited ledger.
func LookupKind(kind Kind) (KindSpec, bool) {
	for _, spec := range kinds {
		if spec.Kind == kind {
			return spec, true
		}
	}
	return KindSpec{}, false
}

// KindOf reads a job's kind.
func KindOf(job Job) Kind { return Kind(job.Type) }

// DefaultTimeout bounds a run whose kind has no configured timeout.
//
// Two minutes, which is what Murtaugh gave both of its jobs. It is enough for a
// digest pass — one GitHub search, a handful of conditional reads, one Slack
// call — and short enough that a wedged one is skipped rather than blocking the
// next tick for the rest of the afternoon.
const DefaultTimeout = 2 * time.Minute

// There is deliberately no MaxTimeout here any more. The upper bound belongs
// where the value enters the process — config.MaxJobTimeout, checked at the
// modal, where somebody is standing to be told — and a second copy in this
// package would be an unreachable check that reads like a live one.

// namePattern is what a job may be called.
//
// Constrained because the name is the job's identity in three places at once:
// the ledger's primary key, the `block_id` of its row on the Home tab, and the
// word in every log line about it. Spaces and punctuation survive none of those
// equally well.
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// ValidateName reports whether name may identify a job.
func ValidateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("a job needs a name")
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%q is not a usable job name: letters, digits, dot, dash and underscore, up to 64 characters", name)
	}
	return nil
}

// GitHubJobName is what the pull-request digest is called when Riggs creates it.
//
// A constant because that job is a SINGLETON: there is one review queue, it is
// the admin's, and the Home tab configures it rather than creating instances of
// it. A second one for a colleague's login would be a different feature —
// per-user jobs, configured by that user — and inventing half of it here by
// letting the name vary would leave two rows silently competing to write the
// same digest.
//
// It is only used for a job Riggs creates from scratch. A job adopted from an
// older ledger keeps whatever it was already called: renaming a row on an
// upgrade would break every log line about it for the sake of tidiness.
const GitHubJobName = "pull-requests-reviewer"

// NewGitHubJob assembles the pull-request digest for one login.
func NewGitHubJob(name, login, spec string) (Job, error) {
	login = strings.TrimSpace(login)
	if login == "" {
		return Job{}, fmt.Errorf("a GitHub username is required: it is whose review queue this fetches")
	}
	if strings.ContainsFunc(login, unicode.IsSpace) {
		// Caught here rather than by GitHub's 404 three minutes later. A login
		// with a space in it is a pasted profile URL or two names in one box,
		// and both are worth saying out loud at the form.
		return Job{}, fmt.Errorf("%q is not a GitHub username: it has a space in it", login)
	}
	return newJob(name, KindGitHubReviews, map[string]string{ParamLogin: login}, spec)
}

// NewJiraJob assembles the ticket digest for one query.
func NewJiraJob(name, jql, spec string) (Job, error) {
	jql = strings.TrimSpace(jql)
	if jql == "" {
		return Job{}, fmt.Errorf("a JQL query is required: it is what decides which tickets are advertised")
	}
	return newJob(name, KindJiraTickets, map[string]string{ParamJQL: jql}, spec)
}

// newJob is the shared half: the checks that are the same whatever the kind.
//
// Everything is checked in one place because there are two front doors — the
// Home tab's modals and `riggs jobs add` — and a rule enforced in only one of
// them is a rule that is not enforced.
//
// A new job is always ENABLED. The old signature took it as a parameter and
// both callers passed true; the one place the answer is genuinely "no" is an
// edit of an existing job, where it is carried over from the row rather than
// asked for (see apphome.saveJob), because Disable is a menu control and not a
// form field.
func newJob(name string, kind Kind, params map[string]string, spec string) (Job, error) {
	name = strings.TrimSpace(name)
	if err := ValidateName(name); err != nil {
		return Job{}, err
	}
	if _, known := LookupKind(kind); !known {
		return Job{}, fmt.Errorf("job %s: %q is not a kind of job this build runs", name, kind)
	}
	if _, err := Parse(spec); err != nil {
		return Job{}, fmt.Errorf("job %s: %w", name, err)
	}
	return Job{
		Name: name, Type: string(kind), Params: params,
		Spec: strings.TrimSpace(spec), Enabled: true,
	}, nil
}

// Param reads one of a job's parameters.
func Param(job Job, name string) string {
	if job.Params == nil {
		return ""
	}
	return job.Params[name]
}

// Args renders the argument list a job runs as.
//
// This is the ONLY place the command spellings are written down for the
// scheduler, and they are a contract with internal/frontends/cli: the child
// process resolves `git pr --bulk` and `jira tickets --bulk` by exact match, so
// a rename on either side does not fail to build — it fails at 3am, in a job,
// with "unknown command".
//
// The JQL goes in as ONE argument, whatever is in it. That is the whole reason
// this function exists rather than a stored string being split: a query is full
// of spaces and quotes, and every layer that re-splits it is a layer that can
// lose it.
func Args(job Job) ([]string, error) {
	switch KindOf(job) {
	case KindGitHubReviews:
		login := Param(job, ParamLogin)
		if login == "" {
			return nil, fmt.Errorf("job %s has no GitHub username to fetch reviews for", job.Name)
		}
		return []string{"git", "pr", "--bulk", login}, nil
	case KindJiraTickets:
		jql := Param(job, ParamJQL)
		if jql == "" {
			return nil, fmt.Errorf("job %s has no JQL query to run", job.Name)
		}
		return []string{"jira", "tickets", "--bulk", jql}, nil
	default:
		return nil, fmt.Errorf("job %s is of kind %q, which this build does not run", job.Name, job.Type)
	}
}

// Command renders a job's arguments as the line somebody would type.
//
// For display only, now that nothing reads a command line back in. It is still
// quoted properly — a JQL rendered bare on the Home tab reads as several
// arguments, and the row is the one place an operator checks what a job
// actually runs.
//
// A job whose kind this build does not know renders as empty rather than as an
// error string. The row above it already says the kind is unknown; a second
// copy of the same news in the code slot helps nobody.
func Command(job Job) string {
	args, err := Args(job)
	if err != nil {
		return ""
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = QuoteArg(a)
	}
	return strings.Join(quoted, " ")
}

// QuoteArg renders one argument the way a shell would need it written.
//
// Only the four characters that would change how a reader parses the line force
// quoting — whitespace, both quotes, and the backslash — so ordinary tokens stay
// bare and the line still reads like something a person typed. Single quotes are
// preferred because JQL's own quoting is double, and `'...'` leaves it visible
// rather than burying it under backslashes.
func QuoteArg(arg string) string {
	if arg == "" {
		return "''"
	}
	if !strings.ContainsFunc(arg, needsQuoting) {
		return arg
	}
	if !strings.Contains(arg, "'") {
		return "'" + arg + "'"
	}
	// A single quote cannot be escaped inside single quotes, in this dialect or
	// in sh. Close, emit an escaped one, reopen — the same `'\''` sh uses.
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// needsQuoting reports whether c would change how a reader parses a token.
func needsQuoting(c rune) bool {
	return unicode.IsSpace(c) || c == '\'' || c == '"' || c == '\\'
}
