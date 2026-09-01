package schedule

import "fmt"

// The jobs Riggs takes over from Murtaugh.
//
// They are declared here, in the package that runs them, rather than being read
// out of Murtaugh's database. Riggs cannot see that database's schema and has
// no business learning it: the two definitions below are the ones `riggs
// install` has been registering all along — same names, same commands, same
// cadence — so materialising them here reproduces exactly what was running,
// from a source this repository can be held to.
//
// The names are deliberately the ones Murtaugh used. Not for compatibility —
// nothing reads them across the boundary — but because they are what the
// operator has been looking at, and a migration that also renames everything
// makes it impossible to tell a moved job from a new one.

// DefaultTicketJQL is the query the ticket digest has always advertised.
const DefaultTicketJQL = `project = NYX AND labels = "ai-able" AND assignee IS EMPTY AND status = "Ready"`

// AdoptedNames are the Murtaugh job names Riggs replaces, so a migration can
// tell the operator exactly what to remove.
var AdoptedNames = []string{"github-review-queue", "quick-coding-tasks-poll"}

// Adopted builds the job set Riggs takes over.
//
// login is the GitHub user whose review queue the digest mirrors. It is a
// parameter rather than a setting for the reason §14 gives at length: as
// configuration it was resolved at run time, so an edit could repoint the queue
// at a different person while the job looked unchanged. Here it goes into the
// job's own argument list, where reading the job answers the question.
//
// An empty login yields only the ticket job, and says so, rather than a review
// digest that fetches nobody's reviews.
func Adopted(login, jql string) ([]Job, []string, error) {
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
