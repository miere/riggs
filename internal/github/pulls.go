package github

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Detail is one pull request as the REST API reports it.
//
// GraphQL answered all of this in a single query; REST needs several calls
// (§8). The trade is deliberate: each is individually cacheable, and the
// steady state of the loop is "nothing changed".
type Detail struct {
	Repo   string
	Number int
	Title  string
	Body   string
	URL    string
	Author string
	Draft  bool
	// State is "open" or "closed"; Merged distinguishes the two closures.
	State  string
	Merged bool
	// CreatedAt orders the bulk digest, which is FIFO by pull request age
	// (§9b) — oldest waiting first.
	CreatedAt *time.Time
	MergedAt  *time.Time
	ClosedAt  *time.Time
	HeadSHA   string
	// RequestedUsers are the logins still awaiting review. GitHub removes a
	// reviewer from this list the moment they review, which is what makes the
	// queue non-sticky without storing a flag.
	RequestedUsers []string
	// RequestedTeams are team-based requests. A team request alone is not a
	// direct request, and the Python mirror deliberately ignores those.
	RequestedTeams []string
	// Archived reports that the repository is archived, i.e. read-only.
	// GitHub then locks every pull request in it and answers a review with
	// `422 lock prevents review`. It rides along in this payload
	// (`base.repo.archived`), so knowing it costs no extra request.
	Archived bool
}

// Requested reports whether login is a *direct* requested reviewer.
func (d Detail) Requested(login string) bool {
	for _, u := range d.RequestedUsers {
		if strings.EqualFold(u, login) {
			return true
		}
	}
	return false
}

// Ref renders owner/repo#number.
func (d Detail) Ref() string { return fmt.Sprintf("%s#%d", d.Repo, d.Number) }

// PullRequestDetail fetches one pull request.
func (c *Client) PullRequestDetail(ctx context.Context, repo string, number int) (Detail, error) {
	var raw struct {
		Number    int        `json:"number"`
		Title     string     `json:"title"`
		Body      string     `json:"body"`
		HTMLURL   string     `json:"html_url"`
		Draft     bool       `json:"draft"`
		State     string     `json:"state"`
		Merged    bool       `json:"merged"`
		CreatedAt *time.Time `json:"created_at"`
		MergedAt  *time.Time `json:"merged_at"`
		ClosedAt  *time.Time `json:"closed_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Repo struct {
				Archived bool `json:"archived"`
			} `json:"repo"`
		} `json:"base"`
		RequestedReviewers []struct {
			Login string `json:"login"`
		} `json:"requested_reviewers"`
		RequestedTeams []struct {
			Slug string `json:"slug"`
		} `json:"requested_teams"`
	}
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), &raw); err != nil {
		return Detail{}, err
	}

	d := Detail{
		Repo: repo, Number: raw.Number, Title: raw.Title, Body: raw.Body,
		URL: raw.HTMLURL, Author: raw.User.Login, Draft: raw.Draft,
		State: raw.State, Merged: raw.Merged, CreatedAt: raw.CreatedAt,
		MergedAt: raw.MergedAt, ClosedAt: raw.ClosedAt, HeadSHA: raw.Head.SHA,
		Archived: raw.Base.Repo.Archived,
	}
	for _, u := range raw.RequestedReviewers {
		d.RequestedUsers = append(d.RequestedUsers, u.Login)
	}
	for _, t := range raw.RequestedTeams {
		d.RequestedTeams = append(d.RequestedTeams, t.Slug)
	}
	return d, nil
}

// Review is one submitted review.
type Review struct {
	Author      string
	State       string // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED
	SubmittedAt time.Time
}

// Reviews returns the *latest* review per author, which is what decides the
// verdict — GitHub keeps one standing review per person, and a new commit
// dismisses a prior approval.
//
// REST has no `reviewDecision` field (GraphQL's does), so it is derived from
// these. See LatestByAuthor.
func (c *Client) Reviews(ctx context.Context, repo string, number int) ([]Review, error) {
	var raw []struct {
		State       string    `json:"state"`
		SubmittedAt time.Time `json:"submitted_at"`
		User        struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100", repo, number), &raw); err != nil {
		return nil, err
	}
	out := make([]Review, 0, len(raw))
	for _, r := range raw {
		out = append(out, Review{Author: r.User.Login, State: strings.ToUpper(r.State), SubmittedAt: r.SubmittedAt})
	}
	return LatestByAuthor(out), nil
}

// LatestByAuthor reduces a review list to the standing review per author,
// preserving the order the reviews arrived in.
//
// COMMENTED reviews are ignored: commenting does not change a verdict, and
// treating it as one would let a passing remark collapse a card.
func LatestByAuthor(reviews []Review) []Review {
	latest := map[string]Review{}
	var order []string
	for _, r := range reviews {
		if r.State == "COMMENTED" {
			continue
		}
		if _, seen := latest[r.Author]; !seen {
			order = append(order, r.Author)
		}
		if prior, seen := latest[r.Author]; !seen || !r.SubmittedAt.Before(prior.SubmittedAt) {
			latest[r.Author] = r
		}
	}
	out := make([]Review, 0, len(order))
	for _, a := range order {
		out = append(out, latest[a])
	}
	return out
}

// Check is one entry in the combined status rollup.
type Check struct {
	Name string
	// Status is "queued", "in_progress" or "completed" for a check run; empty
	// for a legacy commit status.
	Status string
	// Conclusion is the check run's outcome, or the status context's state.
	Conclusion string
	// Legacy marks a commit status (as opposed to a check run), because the
	// two use different vocabularies for the same idea.
	Legacy bool
}

// Checks returns every check run and legacy status context on a commit.
//
// Both are needed: a *required* status context that has not reported yet is
// what stops a PR being called green prematurely.
func (c *Client) Checks(ctx context.Context, repo, sha string) ([]Check, error) {
	var runs struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=100", repo, sha), &runs); err != nil {
		return nil, err
	}
	out := make([]Check, 0, len(runs.CheckRuns))
	for _, r := range runs.CheckRuns {
		out = append(out, Check{
			Name:       r.Name,
			Status:     strings.ToUpper(r.Status),
			Conclusion: strings.ToUpper(r.Conclusion),
		})
	}

	var combined struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/commits/%s/status?per_page=100", repo, sha), &combined); err != nil {
		return nil, err
	}
	for _, s := range combined.Statuses {
		out = append(out, Check{Name: s.Context, Conclusion: strings.ToUpper(s.State), Legacy: true})
	}
	return out, nil
}

// ClosedBy reports who closed a pull request, via the issues timeline.
//
// Only used to render "Closed by @X"; any failure degrades to a
// timestamp-only label rather than failing the tick.
func (c *Client) ClosedBy(ctx context.Context, repo string, number int) (string, error) {
	var events []struct {
		Event string `json:"event"`
		Actor struct {
			Login string `json:"login"`
		} `json:"actor"`
	}
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/issues/%d/events?per_page=100", repo, number), &events); err != nil {
		return "", err
	}
	login := ""
	for _, e := range events {
		if e.Event == "closed" {
			login = e.Actor.Login // the last close wins: a PR can be reopened
		}
	}
	return login, nil
}
