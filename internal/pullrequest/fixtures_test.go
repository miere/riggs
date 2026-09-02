package pullrequest

// Shared test fixtures.
//
// They lived in reconcile_test.go until the card loop was deleted; the digest,
// the asker, the approver and the completer all still need a fake GitHub and a
// delivery target, so they moved here rather than being deleted with it.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/miere/riggs-mcp/internal/github"
	"github.com/miere/riggs-mcp/internal/slack"
)

// fakeGH is a scripted GitHub, recording which endpoints were reached so the
// call-avoidance behaviour can be asserted directly.
type fakeGH struct {
	search   []github.PullRequest
	details  map[string]github.Detail
	reviews  map[string][]github.Review
	checks   map[string][]github.Check
	closedBy map[string]string
	// decisions is GitHub's own reviewDecision per ref; absent means none.
	decisions map[string]string
	calls     []string
	failOn    string
}

func (f *fakeGH) record(what string) { f.calls = append(f.calls, what) }

func (f *fakeGH) ReviewRequested(context.Context, string, int) ([]github.PullRequest, error) {
	f.record("search")
	if f.failOn == "search" {
		return nil, errors.New("search exploded")
	}
	return f.search, nil
}

func (f *fakeGH) PullRequestDetail(_ context.Context, repo string, n int) (github.Detail, error) {
	ref := fmt.Sprintf("%s#%d", repo, n)
	f.record("detail:" + ref)
	d, ok := f.details[ref]
	if !ok {
		return github.Detail{}, fmt.Errorf("no such PR %s", ref)
	}
	return d, nil
}

func (f *fakeGH) ReviewDecision(_ context.Context, repo string, n int) (string, error) {
	ref := fmt.Sprintf("%s#%d", repo, n)
	f.record("decision:" + ref)
	return f.decisions[ref], nil
}

func (f *fakeGH) Reviews(_ context.Context, repo string, n int) ([]github.Review, error) {
	ref := fmt.Sprintf("%s#%d", repo, n)
	f.record("reviews:" + ref)
	return f.reviews[ref], nil
}

func (f *fakeGH) Checks(_ context.Context, repo, sha string) ([]github.Check, error) {
	f.record("checks:" + sha)
	return f.checks[sha], nil
}

func (f *fakeGH) ClosedBy(_ context.Context, repo string, n int) (string, error) {
	ref := fmt.Sprintf("%s#%d", repo, n)
	f.record("closedby:" + ref)
	return f.closedBy[ref], nil
}

func (f *fakeGH) called(what string) bool {
	for _, c := range f.calls {
		if c == what {
			return true
		}
	}
	return false
}

var target = slack.Target{Profile: "default", BotToken: "xoxb", Channel: "C1"}

func openPR(ref string) github.Detail {
	repo, n, _ := SplitRef(ref)
	return github.Detail{
		Repo: repo, Number: n, Title: "Fix the thing", Body: "body",
		URL:    "https://github.com/" + repo + "/pull/" + fmt.Sprint(n),
		Author: "alex", State: "open", HeadSHA: "sha-" + ref,
		RequestedUsers: []string{"miere"},
	}
}

func greenGH(ref string) *fakeGH {
	return &fakeGH{
		search:  []github.PullRequest{{Repo: strings.Split(ref, "#")[0], Number: 1}},
		details: map[string]github.Detail{ref: openPR(ref)},
		checks:  map[string][]github.Check{"sha-" + ref: {run("build", "COMPLETED", "SUCCESS")}},
	}
}

const ref = "o/r#1"
