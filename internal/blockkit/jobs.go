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

	// HomeNewJobIntent is the controls-menu option that opens an empty job
	// editor. It sits on `app_menu` beside Restart because it is about Riggs
	// rather than about any one job — there is no row to hang it off when there
	// are no jobs yet, which is exactly when it is needed most.
	HomeNewJobIntent = "new_job"

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
	// Schedule is the cadence as written: "3m", "0 9 * * 1-5".
	Schedule string
	// Command is the argument list as a line: "git pr --bulk miere".
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
			Type:     "context",
			Elements: []textObj{mrkdwn("Nothing is scheduled. Use *New job…* in the menu above.")},
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
		{
			Text:  plainVerbatim(MarkerFailed + "  Delete"),
			Value: HomeJobDeleteIntent,
			// The one control on this surface that destroys something and
			// cannot be undone. An overflow gives no second chance of its own,
			// and "I meant to press Disable" is one row's distance away.
			Confirm: &confirmObj{
				Title:   plain("Delete this job?"),
				Text:    mrkdwn("*" + escapeMrkdwn(j.ID) + "* will stop running and its history will be forgotten. Disable keeps both."),
				Confirm: plain("Delete"),
				Deny:    plain("Keep it"),
				Style:   "danger",
			},
		},
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
