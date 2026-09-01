package installer

import (
	"context"
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/schedule"
)

// The install writes Riggs' own schedule rather than registering it elsewhere.
func TestInstallSchedulesTheAdoptedJobs(t *testing.T) {
	s := happyScript()
	r := newRig(t, s)
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(r.jobs) != 2 {
		t.Fatalf("scheduled %d jobs, want the two Riggs takes over: %+v", len(r.jobs), r.jobs)
	}
	byName := map[string]string{}
	for _, job := range r.jobs {
		byName[job.Name] = schedule.Command(job)
		if job.Spec != "3m" {
			t.Errorf("%s runs at %q, want the cadence it already had", job.Name, job.Spec)
		}
		if !job.Enabled {
			t.Errorf("%s was scheduled disabled", job.Name)
		}
	}
	// The GitHub login goes in the job's own arguments, so reading the job
	// answers whose reviews it fetches.
	if got := byName["github-review-queue"]; !strings.Contains(got, "git pr --bulk miere") {
		t.Errorf("review job = %q", got)
	}
	if got := byName["quick-coding-tasks-poll"]; !strings.Contains(got, "jira tickets --bulk") {
		t.Errorf("ticket job = %q", got)
	}
	// A click is delivered to the app that POSTED the message, so the profile
	// is not cosmetic: a digest sent through the wrong one renders a menu the
	// daemon never hears about.
	if got := byName["github-review-queue"]; !strings.Contains(got, "--slack-profile default") {
		t.Errorf("review job names no profile: %q", got)
	}
	if got := byName["github-review-queue"]; !strings.Contains(got, "--slack-channel C0B24F579T4") {
		t.Errorf("review job names no channel: %q", got)
	}

	// The ledger goes beside the config, so one --config-file moves both.
	if r.dbPath != "/tmp/riggs/config.db" {
		t.Errorf("ledger = %q, want it beside the config", r.dbPath)
	}
}

// It says nothing about any other scheduler. Nothing on this side can see
// another tool's configuration, and a warning describing a state it has not
// checked is how a tool teaches people to ignore its output.
func TestInstallDoesNotAssertWhatAnotherSchedulerHolds(t *testing.T) {
	s := happyScript()
	r := newRig(t, s)
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(strings.ToLower(s.transcript()), "murtaugh jobs remove") {
		t.Errorf("the install claims another scheduler has copies:\n%s", s.transcript())
	}
}

// Declining is an ordinary answer, not a broken install: `riggs jobs import`
// adopts them later.
func TestDecliningTheScheduleLeavesItEmpty(t *testing.T) {
	s := happyScript()
	// The smoke test, then the schedule.
	s.confirms = []bool{true, false}
	r := newRig(t, s)
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(r.jobs) != 0 {
		t.Fatalf("scheduled %d jobs after declining", len(r.jobs))
	}
	if !strings.Contains(s.transcript(), "riggs jobs import") {
		t.Errorf("the skip does not say how to do it later:\n%s", s.transcript())
	}
}

// With no GitHub login there is nobody to fetch reviews for, and a job that
// fetches nobody's is worse than one that is not there.
func TestNoGitHubLoginSkipsTheReviewDigest(t *testing.T) {
	s := happyScript()
	s.answers[1] = "<empty>" // the GitHub login
	r := newRig(t, s)
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, job := range r.jobs {
		if job.Name == "github-review-queue" {
			t.Fatalf("a review digest was scheduled with no login: %+v", job)
		}
	}
	if !strings.Contains(s.transcript(), "nobody to fetch reviews for") {
		t.Errorf("the skip was not reported:\n%s", s.transcript())
	}
}
