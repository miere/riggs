package schedule

import "fmt"

// The standard jobs: the two Riggs schedules for itself unless told otherwise.
//
// They are declared here so that `riggs install` and `riggs jobs seed` create
// the same thing, from one definition, rather than each carrying its own copy
// of a command line that has to stay in step with the tools it invokes.
//
// This is a SEED, not an import. Nothing is read from anywhere: these are the
// two jobs `riggs install` has been setting up all along — same names, same
// commands, same cadence — and the names are kept so a machine that had them
// under another scheduler recognises them rather than gaining two lookalikes.

// DefaultTicketJQL is the query the ticket digest has always advertised.
const DefaultTicketJQL = `project = NYX AND labels = "ai-able" AND assignee IS EMPTY AND status = "Ready"`

// Standard builds the default job set.
//
// login is the GitHub user whose review queue the digest mirrors. It is a
// parameter rather than a setting for the reason §14 gives at length: as
// configuration it was resolved at run time, so an edit could repoint the queue
// at a different person while the job looked unchanged. Here it goes into the
// job's own argument list, where reading the job answers the question.
//
// An empty login yields only the ticket job, and says so, rather than a review
// digest that fetches nobody's reviews.
func Standard(login, jql string) ([]Job, []string, error) {
	if jql == "" {
		jql = DefaultTicketJQL
	}
	var jobs []Job
	var skipped []string

	if login == "" {
		skipped = append(skipped,
			"github-review-queue (no GitHub login was given, so there is nobody to fetch reviews for)")
	} else {
		job, err := NewJob("github-review-queue",
			[]string{"git", "pr", "--bulk", login}, "3m", DefaultTimeout, true)
		if err != nil {
			return nil, nil, fmt.Errorf("building the review digest job: %w", err)
		}
		jobs = append(jobs, job)
	}

	tickets, err := NewJob("quick-coding-tasks-poll",
		[]string{"jira", "tickets", "--bulk", jql}, "3m", DefaultTimeout, true)
	if err != nil {
		return nil, nil, fmt.Errorf("building the ticket digest job: %w", err)
	}
	return append(jobs, tickets), skipped, nil
}
