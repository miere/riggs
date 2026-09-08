package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/schedule"
)

func cliStore(t *testing.T) *notify.Store {
	t.Helper()
	s, err := notify.Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// The kind is the first word and it is not optional. This used to take a
// command line, which made the operator responsible for the spelling of a
// command the binary already knows — and made a typo in it a job that fails
// every three minutes rather than a usage error at the prompt.
func TestAddJobTakesAKind(t *testing.T) {
	ctx, store := context.Background(), cliStore(t)
	const jql = `project = NYX AND status = "Ready"`

	if err := addJob(ctx, store, []string{"github", "reviews", "3m", "miere"}); err != nil {
		t.Fatalf("addJob github: %v", err)
	}
	if err := addJob(ctx, store, []string{"jira", "tickets", "0 9 * * 1-5", jql}); err != nil {
		t.Fatalf("addJob jira: %v", err)
	}

	reviews, found, _ := store.Job(ctx, "reviews")
	if !found || schedule.KindOf(reviews) != schedule.KindGitHubReviews {
		t.Fatalf("reviews = %+v", reviews)
	}
	tickets, found, _ := store.Job(ctx, "tickets")
	if !found {
		t.Fatal("the jira job was not created")
	}
	// The query is stored as ONE parameter, quoting and all: the shell worked
	// out where it started and ended, and nothing re-splits it.
	if got := schedule.Param(tickets, schedule.ParamJQL); got != jql {
		t.Fatalf("jql = %q, want it verbatim", got)
	}
	if tickets.Spec != "0 9 * * 1-5" {
		t.Fatalf("spec = %q", tickets.Spec)
	}
}

// An unquoted JQL is the mistake this catches: the shell splits it and only the
// first word arrives. Refused rather than joined back up, because joining is a
// guess at the operator's spacing and the failure would be silent.
func TestAddJobRefusesAnUnquotedQuery(t *testing.T) {
	ctx, store := context.Background(), cliStore(t)

	err := addJob(ctx, store, []string{"jira", "tickets", "3m", "project", "=", "NYX"})
	if err == nil {
		t.Fatal("an unquoted query was accepted")
	}
	if !strings.Contains(err.Error(), "quote the whole value") {
		t.Fatalf("err = %v, want it to say what to do", err)
	}
	if _, found, _ := store.Job(ctx, "tickets"); found {
		t.Fatal("the job was created anyway")
	}
}

func TestAddJobRefusesWhatItCannotSchedule(t *testing.T) {
	ctx, store := context.Background(), cliStore(t)

	for name, args := range map[string][]string{
		"an unknown kind":  {"slack", "digest", "3m", "C1"},
		"too few words":    {"github", "reviews", "3m"},
		"a bad schedule":   {"github", "reviews", "weekly", "miere"},
		"an unusable name": {"github", "my job", "3m", "miere"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := addJob(ctx, store, args); err == nil {
				t.Fatalf("addJob accepted %q", args)
			}
		})
	}
}

// A name already in use would silently replace somebody else's job.
func TestAddJobRefusesADuplicate(t *testing.T) {
	ctx, store := context.Background(), cliStore(t)
	if err := addJob(ctx, store, []string{"github", "reviews", "3m", "miere"}); err != nil {
		t.Fatalf("addJob: %v", err)
	}
	err := addJob(ctx, store, []string{"jira", "reviews", "3m", "project = NYX"})
	if err == nil {
		t.Fatal("a duplicate name was accepted")
	}
	if !strings.Contains(err.Error(), "already a job") {
		t.Fatalf("err = %v", err)
	}
}

// The terminal is where an upgrade can be inspected without restarting the
// daemon to find out what it would throw away.
func TestMigrateFromTheCommandLine(t *testing.T) {
	ctx, store := context.Background(), cliStore(t)
	if err := addJob(ctx, store, []string{"github", "reviews", "3m", "miere"}); err != nil {
		t.Fatalf("addJob: %v", err)
	}
	// Already typed, so there is nothing to do — and saying so is not an error.
	if err := migrateJobs(ctx, store); err != nil {
		t.Fatalf("migrateJobs: %v", err)
	}
	if job, _, _ := store.Job(ctx, "reviews"); schedule.KindOf(job) != schedule.KindGitHubReviews {
		t.Fatalf("the pass changed a typed job: %+v", job)
	}
}
