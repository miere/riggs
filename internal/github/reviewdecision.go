package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Review decisions, as GraphQL reports them.
const (
	DecisionApproved         = "APPROVED"
	DecisionChangesRequested = "CHANGES_REQUESTED"
	DecisionReviewRequired   = "REVIEW_REQUIRED"
)

// ReviewDecision returns GitHub's own verdict on a pull request: APPROVED,
// CHANGES_REQUESTED, REVIEW_REQUIRED, or "" when GitHub reports none.
//
// This is the one field REST does not expose, and it is deliberately not
// reconstructed from the review list. Two attempts to derive it were both
// wrong, in opposite directions, and the parity check caught both against live
// data:
//
//   - gcp-jsm-bridge#80 — one approval from another reviewer, branch
//     unprotected, we are still requested: GitHub says APPROVED.
//   - nct-intelligence-beholder#1315 — the same shape, plus an older dismissed
//     review: GitHub says null.
//
// Whatever separates those is not documented, so it is asked rather than
// guessed. See ARCHITECTURE.md §8.
//
// The cost is one GraphQL point per call — against 164 per tick for the
// `gh pr list` this replaces. Everything else still goes over REST with
// conditional requests.
func (c *Client) ReviewDecision(ctx context.Context, repo string, number int) (string, error) {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return "", fmt.Errorf("github: not an owner/name repository: %q", repo)
	}

	body, err := json.Marshal(map[string]any{
		"query": `query($owner:String!,$name:String!,$number:Int!){
			repository(owner:$owner,name:$name){
				pullRequest(number:$number){ reviewDecision }
			}
		}`,
		"variables": map[string]any{"owner": owner, "name": name, "number": number},
	})
	if err != nil {
		return "", fmt.Errorf("github: encoding graphql query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/graphql", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("github: building graphql request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: graphql: %w", err)
	}
	defer resp.Body.Close()
	c.stats.Requests++
	c.stats.GraphQL++

	var payload struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewDecision *string `json:"reviewDecision"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("github: decoding graphql response: %w", err)
	}
	if len(payload.Errors) > 0 {
		return "", fmt.Errorf("github: graphql: %s", payload.Errors[0].Message)
	}
	if d := payload.Data.Repository.PullRequest.ReviewDecision; d != nil {
		return *d, nil
	}
	// null is meaningful: GitHub has no verdict, which is NOT the same as
	// "approved". Collapsing the two is precisely the bug this call removes.
	return "", nil
}

// splitRepo divides "owner/name".
func splitRepo(repo string) (owner, name string, ok bool) {
	for i := 0; i < len(repo); i++ {
		if repo[i] == '/' {
			return repo[:i], repo[i+1:], i > 0 && i < len(repo)-1
		}
	}
	return "", "", false
}
