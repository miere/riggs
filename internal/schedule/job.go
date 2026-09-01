package schedule

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/miere/riggs-mcp/internal/notify"
)

// Job is the record the scheduler works from. It is the ledger's row (§9c),
// used directly rather than copied into a parallel struct that would have to be
// kept in step with it.
type Job = notify.Job

// DefaultTimeout bounds a run whose job did not say.
//
// Two minutes, which is what Murtaugh gave both of its jobs. It is enough for a
// digest pass — one GitHub search, a handful of conditional reads, one Slack
// call — and short enough that a wedged one is skipped rather than blocking the
// next tick for the rest of the afternoon.
const DefaultTimeout = 2 * time.Minute

// MaxTimeout is the longest a job may be given.
//
// An hour. Not a technical limit: a job that needs longer than an hour is not a
// scheduled task, it is a service, and it should be supervised as one rather
// than restarted from a ticker every time it fails to finish.
const MaxTimeout = time.Hour

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

// NewJob validates and assembles a job from what a modal or a command line
// supplied.
//
// args is the argument list for the riggs binary, already split. spec is the
// schedule in either dialect. A zero timeout takes DefaultTimeout.
//
// Everything is checked here, in one place, because there are two front doors —
// the Home tab's modal and `riggs jobs add` — and a rule enforced in only one
// of them is a rule that is not enforced.
func NewJob(name string, args []string, spec string, timeout time.Duration, enabled bool) (Job, error) {
	if err := ValidateName(name); err != nil {
		return Job{}, err
	}
	if len(args) == 0 {
		return Job{}, fmt.Errorf("job %s has nothing to run (e.g. `git pr --bulk miere`)", name)
	}
	if _, err := Parse(spec); err != nil {
		return Job{}, fmt.Errorf("job %s: %w", name, err)
	}
	switch {
	case timeout == 0:
		timeout = DefaultTimeout
	case timeout < 0:
		return Job{}, fmt.Errorf("job %s: a timeout cannot be negative", name)
	case timeout > MaxTimeout:
		return Job{}, fmt.Errorf("job %s: %s is longer than the %s maximum; something that runs that long is a service, not a job",
			name, timeout, MaxTimeout)
	}
	return Job{
		Name: name, Args: args, Spec: strings.TrimSpace(spec),
		Timeout: timeout, Enabled: enabled,
	}, nil
}

// SplitArgs reads a command line into an argument list.
//
// Split on whitespace, with no quote handling — the same rule, and the same
// reasoning, as `ai.command` (§7bb): anything needing more than that wants a
// wrapper script, which is one line and legible from the outside, rather than a
// quoting dialect invented in a Slack modal.
//
// A leading `riggs` is dropped. The binary is not the operator's to choose —
// every job runs THIS build, at the path this daemon was started from — and
// typing the whole command Murtaugh used to run is the obvious thing to do.
func SplitArgs(command string) []string {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) > 0 && strings.EqualFold(fields[0], "riggs") {
		fields = fields[1:]
	}
	return fields
}

// Command renders a job's arguments as the line somebody would type.
func Command(job Job) string { return strings.Join(job.Args, " ") }
