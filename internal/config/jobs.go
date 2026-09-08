package config

import (
	"fmt"
	"strings"
	"time"
)

// How long a run of each kind of job may take, named so a surface can list and
// edit them without knowing where in the YAML each one lives.
//
// This is the reaction registry's third sibling (reactions.go, prompts.go), and
// the same shape for the same reason: the Configuration modal edits these the
// way the Customisation modal edits those.
//
// A timeout used to live on the job itself, typed into the editor beside the
// command. It moved here because it is not a property of the particular job:
// "how long may a ticket digest take" is a fact about ticket digests, and
// asking it once per job meant four boxes to keep in step and a fifth answer —
// whatever an operator typed the day they created it — that nothing ever
// revisited.
//
// The ids are DELIBERATELY not schedule's Kind values, even though they are
// spelled identically today. config knows there are two kinds of job and
// nothing about what either one does; schedule knows the kinds and nothing
// about YAML. The switch that joins the two vocabularies lives in the
// composition root (app/daemon.go), exactly like the one that joins a
// communication state to its emoji — and it is a switch rather than a string
// conversion so the first divergence is a compile error rather than a setting
// that silently stops applying.

// JobTimeoutID names one configurable timeout. It is a bare token: it rides in
// a modal's block_id and the router matches it exactly (§7b).
type JobTimeoutID string

const (
	// JobTimeoutGitHub bounds a pull-request digest pass.
	JobTimeoutGitHub JobTimeoutID = "github-reviews"
	// JobTimeoutJira bounds a ticket digest pass.
	JobTimeoutJira JobTimeoutID = "jira-tickets"
)

// DefaultJobTimeout is what an unconfigured kind gets.
//
// Two minutes, which is what Murtaugh gave both of its jobs and what
// schedule.DefaultTimeout has always meant. It is enough for a digest pass —
// one search, a handful of conditional reads, one Slack call — and short enough
// that a wedged one is skipped rather than blocking the next tick for the rest
// of the afternoon.
const DefaultJobTimeout = 2 * time.Minute

// MaxJobTimeout is the longest any kind may be given.
//
// An hour, and enforced HERE because this is the door a human types into: a job
// that needs longer than an hour is not a scheduled task, it is a service, and
// it should be supervised as one rather than restarted from a ticker every time
// it fails to finish. The scheduler carries no second copy of this bound — an
// unreachable check that reads like a live one is worse than none.
const MaxJobTimeout = time.Hour

// Jobs holds the per-kind settings. Every field is a duration as written —
// "2m", "45s" — and an empty one means the default is in force.
//
// Strings rather than time.Duration because the file is the source: a value
// that fails to parse must be reportable as "45x is not a duration", not
// silently zeroed by the YAML unmarshaller into a job with no time at all.
type Jobs struct {
	GitHubTimeout string `yaml:"github-timeout"`
	JiraTimeout   string `yaml:"jira-timeout"`
}

// JobTimeoutSpec describes one configurable timeout.
type JobTimeoutSpec struct {
	// ID is the token surfaces address it by.
	ID JobTimeoutID
	// Label is the human name, shown in the Configuration modal.
	Label string
	// Hint says what the bound applies to, shown under the modal's input.
	Hint string
	// Path is where the value lives in the YAML file, as a sequence of mapping
	// keys. The writer creates whatever is missing along it.
	Path []string
	// Default is what an unset timeout resolves to.
	Default time.Duration

	// get reads the RAW configured value — empty when the default is in force,
	// which is the distinction the writer needs: clearing the box deletes the
	// key rather than writing today's default into the file.
	get func(*Config) string
	// set writes the value back into the loaded Config, so a running daemon
	// bounds its next run by the edited timeout without being restarted.
	set func(*Config, string)
}

// jobTimeouts is the registry, in the order the Home tab lists the kinds.
var jobTimeouts = []JobTimeoutSpec{
	{
		ID:      JobTimeoutGitHub,
		Label:   "Pull requests timeout",
		Hint:    "How long one pass of the pull-request digest may take, e.g. 2m.",
		Path:    []string{"jobs", "github-timeout"},
		Default: DefaultJobTimeout,
		get:     func(c *Config) string { return c.Jobs.GitHubTimeout },
		set:     func(c *Config, v string) { c.Jobs.GitHubTimeout = v },
	},
	{
		ID:      JobTimeoutJira,
		Label:   "Jira tickets timeout",
		Hint:    "How long one pass of the ticket digest may take, e.g. 2m.",
		Path:    []string{"jobs", "jira-timeout"},
		Default: DefaultJobTimeout,
		get:     func(c *Config) string { return c.Jobs.JiraTimeout },
		set:     func(c *Config, v string) { c.Jobs.JiraTimeout = v },
	},
}

// JobTimeoutSpecs lists the configurable timeouts, in rendering order.
//
// A copy, because a caller iterating this to draw a modal has no business
// reaching back through the slice header into the registry.
func JobTimeoutSpecs() []JobTimeoutSpec {
	out := make([]JobTimeoutSpec, len(jobTimeouts))
	copy(out, jobTimeouts)
	return out
}

// LookupJobTimeout finds one by id. ok is false for a token that names nothing,
// which is what a modal opened by an older build looks like.
func LookupJobTimeout(id JobTimeoutID) (JobTimeoutSpec, bool) {
	for _, s := range jobTimeouts {
		if s.ID == id {
			return s, true
		}
	}
	return JobTimeoutSpec{}, false
}

// JobTimeout is the effective bound for one kind: what is configured, or the
// default.
//
// A value that will not parse resolves to the DEFAULT rather than to zero or to
// an error. It cannot be rejected at this end of the call — the scheduler is
// about to run something and has nobody to tell — and the two readings of a
// broken setting are "no timeout at all" and "the default"; only one of them
// lets a job run forever. It is refused at the modal, which is where a human is
// standing.
func (c *Config) JobTimeout(id JobTimeoutID) time.Duration {
	spec, ok := LookupJobTimeout(id)
	if !ok {
		return DefaultJobTimeout
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	raw := strings.TrimSpace(spec.get(c))
	if raw == "" {
		return spec.Default
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 || d > MaxJobTimeout {
		return spec.Default
	}
	return d
}

// JobTimeoutText is the bound as WRITTEN, for pre-filling the modal.
//
// The effective value, not the raw one: an admin opening the form to change how
// long a digest gets wants to see the number in force, and "empty means two
// minutes" is a distinction the writer needs and the reader does not.
func (c *Config) JobTimeoutText(id JobTimeoutID) string {
	return c.JobTimeout(id).String()
}

// JobTimeoutOverridden reports whether this timeout is configured rather than
// running on its default.
func (c *Config) JobTimeoutOverridden(id JobTimeoutID) bool {
	spec, ok := LookupJobTimeout(id)
	if !ok {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return strings.TrimSpace(spec.get(c)) != ""
}

// SetJobTimeout rewrites one timeout in the config file and in this Config.
//
// An empty value RESETS it, on the same rule SetPrompt and SetReaction follow:
// the key is deleted rather than filled with today's default, so "never
// configured" stays distinguishable from "configured to whatever the default
// said at the time".
func (c *Config) SetJobTimeout(id JobTimeoutID, raw string) error {
	spec, ok := LookupJobTimeout(id)
	if !ok {
		return fmt.Errorf("config: %q is not a configurable job timeout", id)
	}
	raw = strings.TrimSpace(raw)
	if err := ValidateJobTimeout(raw); err != nil {
		return err
	}
	return c.setScalarSetting(spec.Path, raw, true, func(cfg *Config) { spec.set(cfg, raw) })
}

// ValidateJobTimeout reports whether raw is a duration a job may be given.
//
// Empty is valid: it is how a reset is spelled.
//
// The bounds are checked here, at the modal, and that is the point of the
// function. A zero is a job that is killed before it starts; a negative one is
// the same with a stranger message; an hour and a half is a service pretending
// to be a job. All three would otherwise be discovered by a digest that stopped
// working, hours later, with a timeout as the only clue.
func ValidateJobTimeout(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("%q is not a duration (e.g. 2m, 90s)", raw)
	}
	if d <= 0 {
		return fmt.Errorf("%s is not a usable timeout: a job needs some time to run in", raw)
	}
	if d > MaxJobTimeout {
		return fmt.Errorf("%s is longer than the %s maximum; something that runs that long is a service, not a job",
			raw, MaxJobTimeout)
	}
	return nil
}
