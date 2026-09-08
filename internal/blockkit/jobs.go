package blockkit

import "strings"

// The Jobs section of the App Home tab: what Riggs is running on a schedule,
// and the controls that change it.
//
// It is the surface that replaces reading Murtaugh's config to find out what is
// running. That is most of the reason it exists: a schedule you cannot see is
// one you assume is working, and the two jobs Riggs took over were invisible
// unless you went and looked in another tool's database.
//
// Structurally it is the prompt rows again (§7e): one section per item, the
// item's identity in the block_id, one overflow per row whose option values are
// bare tokens. That is not a coincidence to be factored out — it is the pattern
// this surface has, and a shared renderer would have to grow a flag for every
// place the two diverge, starting with the confirmation on Delete.

const (
	// HomeJobActionID is the action_id of the overflow beside each job.
	HomeJobActionID = "app_job"
	// The intents on it. Bare tokens, matched exactly by the router (§7b);
	// which job they are about rides in the row's block_id.
	HomeJobEditIntent   = "edit"
	HomeJobRunIntent    = "run"
	HomeJobToggleIntent = "toggle"
	HomeJobDeleteIntent = "delete"
	// HomeJobBlockPrefix namespaces a job row's block_id.
	HomeJobBlockPrefix = "job:"

	// The two controls-menu options that create a job. They sit on `app_menu`
	// beside Restart because they are about Riggs rather than about any one job
	// — there is no row to hang them off when there are no jobs yet, which is
	// exactly when they are needed most.
	//
	// Two of them because there are two kinds of job and they are not the same
	// shape of thing (§9d). The GitHub digest is a SINGLETON — one review queue,
	// the admin's — so its option CONFIGURES the one that exists, checkbox and
	// all; the ticket digest is one job per query, so its option creates
	// another. One "New job…" leading to a form with a free-text command box
	// served neither.
	HomeGitHubJobIntent  = "github_job"
	HomeNewJiraJobIntent = "new_jira_job"

	// homeJobCommandLimit is how much of a job's command line the row shows.
	homeJobCommandLimit = 90
	homeJobCommandKeep  = 87
)

// HomeJob is one scheduled job as the Home tab draws it.
//
// Every field is already rendered. This type lays blocks out; working out that
// a job last ran four minutes ago and is next due in fifty seconds is
// arithmetic on a clock, and a package that renders JSON has no business
// holding one.
type HomeJob struct {
	// ID is the job's name, and its identity in the block_id.
	ID string
	// Kind is what sort of job it is, already in human words: "Jira tickets".
	//
	// Rendered because a typed job's parameter no longer says it. `miere` on a
	// row is a GitHub login only if you already know which job you are looking
	// at, and a query beginning `project = NYX` could be several things. It is
	// empty for a job whose kind this build does not know, which the row then
	// says outright — see text().
	Kind string
	// Schedule is the cadence as written: "3m", "0 9 * * 1-5".
	Schedule string
	// Command is the argument list as a line: "git pr --bulk miere".
	//
	// Still the argv rather than the parameter alone, because the row is where
	// an operator checks what a job actually runs — and the answer to "why is
	// this failing" is more often in the command than in the schedule.
	Command string
	// Status is the already-rendered outcome line, marker and all.
	Status string
	// Enabled decides whether the menu offers Disable or Enable. A disabled job
	// keeps every other control: it is paused, not broken.
	Enabled bool
}

// jobBlocks renders the Jobs section, header and all.
//
// Above the prompts rather than below, because it answers the question somebody
// opens this tab to ask. A prompt is read when you are about to change it; a
// schedule is read when you are wondering whether anything is running.
func (h Home) jobBlocks() []any {
	if !h.Admin || !h.ShowJobs {
		return nil
	}
	blocks := []any{
		dividerBlock{Type: "divider"},
		headerBlock{Type: "header", Text: plainEmoji("Jobs"), Level: 1},
	}
	if len(h.Jobs) == 0 {
		// An empty state, not an empty section. "Nothing is scheduled" is a
		// fact worth rendering — the alternative reads as a section that failed
		// to load, and the way to fix it is in the menu directly above.
		blocks = append(blocks, contextBlock{
			Type: "context",
			Elements: []textObj{mrkdwn(
				"Nothing is scheduled. Use *Configure GitHub Jobs…* or *Configure a New Jira Job…* in the menu above.")},
		})
		return blocks
	}
	for _, job := range h.Jobs {
		blocks = append(blocks, job.block())
	}
	return blocks
}

// block renders one job row.
func (j HomeJob) block() accessorySection {
	toggle := MarkerWarning + "  Disable"
	if !j.Enabled {
		toggle = MarkerDone + "  Enable"
	}
	options := []menuOptionObj{
		{Text: plainVerbatim(MarkerAsk + "  Edit"), Value: HomeJobEditIntent},
		{Text: plainVerbatim(MarkerRun + "  Run now"), Value: HomeJobRunIntent},
		{Text: plainVerbatim(toggle), Value: HomeJobToggleIntent},
		// The one control on this surface that destroys something and cannot be
		// undone, and the only one that does not act on the click. Slack has no
		// per-option confirmation to hang here — `confirm` belongs to the
		// overflow element, where it would guard Edit and Run as well — so the
		// second chance is a modal (JobDeleteModal), opened by the handler.
		{Text: plainVerbatim(MarkerFailed + "  Delete"), Value: HomeJobDeleteIntent},
	}
	return accessorySection{
		Type:      "section",
		BlockID:   HomeJobBlockPrefix + j.ID,
		Text:      mrkdwn(j.text()),
		Accessory: &menuElem{Type: "overflow", ActionID: HomeJobActionID, Options: options},
	}
}

// text is the row body: what the job is, what it runs, and how it went.
func (j HomeJob) text() string {
	name := "*" + escapeMrkdwn(j.ID) + "*"
	if kind := strings.TrimSpace(j.Kind); kind != "" {
		name += "  " + escapeMrkdwn(kind)
	} else {
		// A job stored by a newer Riggs, or a hand-edited row. Said on the row
		// rather than left as a gap: every control on this menu still works,
		// including Run now, and the run will fail with the same news at three
		// in the morning if the row does not say it here.
		name += "  " + MarkerWarning + " unknown kind"
	}
	if schedule := strings.TrimSpace(j.Schedule); schedule != "" {
		name += "  _" + escapeMrkdwn(schedule) + "_"
	}
	lines := []string{name}
	if command := strings.TrimSpace(j.Command); command != "" {
		// Backticked rather than escaped: a command line is full of dashes and
		// slashes that mrkdwn would otherwise take an interest in, and code
		// formatting is also what it IS.
		lines = append(lines, "`"+Truncate(command, homeJobCommandLimit, homeJobCommandKeep)+"`")
	}
	if status := strings.TrimSpace(j.Status); status != "" {
		lines = append(lines, status)
	}
	return strings.Join(lines, "\n")
}
