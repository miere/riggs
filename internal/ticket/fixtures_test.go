package ticket

// Shared test fixtures.
//
// They lived in engine_test.go until the Engine was deleted; the digest and
// the assistance request both still need a fake Jira and a delivery target, so
// they moved here rather than going with it.

import (
	"context"
	"errors"
	"time"

	"github.com/miere/riggs-mcp/internal/jira"
	"github.com/miere/riggs-mcp/internal/slack"
)

// fakeJira scripts the Jira seam.
type fakeJira struct {
	search    []jira.Issue
	searchErr error
	issues    map[string]jira.Issue
	getErr    error
}

func (f *fakeJira) Search(context.Context, string, int) ([]jira.Issue, error) {
	return f.search, f.searchErr
}

func (f *fakeJira) Get(_ context.Context, key string) (jira.Issue, error) {
	if f.getErr != nil {
		return jira.Issue{}, f.getErr
	}
	i, ok := f.issues[key]
	if !ok {
		return jira.Issue{}, errors.New("no such issue " + key)
	}
	return i, nil
}

func (f *fakeJira) BrowseURL(key string) string { return "https://jira.test/browse/" + key }

var target = slack.Target{Profile: "default", BotToken: "xoxb", Channel: "C1"}

const adminID = "U0B20G0ET9T"

func ready(key, summary string) jira.Issue {
	return jira.Issue{Key: key, Summary: summary, Status: ReadyStatus,
		Description: "do the thing", Updated: time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC)}
}
