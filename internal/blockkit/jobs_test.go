package blockkit

import (
	"encoding/json"
	"strings"
	"testing"
)

func jobRows() []HomeJob {
	return []HomeJob{
		{ID: "github-review-queue", Kind: "Pull Requests — Reviewer", Schedule: "3m",
			Command: "git pr --bulk miere",
			Status:  MarkerDone + " ran 2m ago in 1.4s · next in 58s", Enabled: true},
		{ID: "nightly", Kind: "Jira tickets", Schedule: "0 9 * * 1-5",
			Command: "jira tickets --bulk 'project = NYX'",
			Status:  MarkerWarning + " disabled", Enabled: false},
	}
}

func TestHomeRendersTheJobRows(t *testing.T) {
	blocks := homeBlocks(t, Home{Version: "v1", Admin: true, ShowJobs: true, Jobs: jobRows()})

	if got := strings.Join(blockTypes(blocks), ","); got != "image,section,divider,header,section,section" {
		t.Fatalf("blocks = %s", got)
	}
	if header := blocks[3]["text"].(map[string]any); header["text"] != "Jobs" {
		t.Fatalf("header = %v", header)
	}
	row := blocks[4]
	if row["block_id"] != HomeJobBlockPrefix+"github-review-queue" {
		t.Fatalf("block_id = %v, want the job's identity", row["block_id"])
	}
	text := row["text"].(map[string]any)["text"].(string)
	for _, want := range []string{"*github-review-queue*", "Pull Requests", "_3m_",
		"`git pr --bulk miere`", "ran 2m ago"} {
		if !strings.Contains(text, want) {
			t.Errorf("row text = %q, want it to contain %q", text, want)
		}
	}
}

// "Nothing is scheduled" is a fact worth rendering; an empty section reads as
// one that failed to load, and the way to fix it is in the menu directly above.
func TestAnEmptyScheduleSaysSo(t *testing.T) {
	blocks := homeBlocks(t, Home{Version: "v1", Admin: true, ShowJobs: true})
	if got := strings.Join(blockTypes(blocks), ","); got != "image,section,divider,header,context" {
		t.Fatalf("blocks = %s", got)
	}
	elements := blocks[4]["elements"].([]any)
	if text := elements[0].(map[string]any)["text"].(string); !strings.Contains(text, "Configure GitHub Jobs") {
		t.Fatalf("the empty state does not point at the control: %q", text)
	}
}

// A build that cannot schedule anything draws no section at all, which is a
// different fact from "nothing is scheduled".
func TestNoSchedulerMeansNoJobsSection(t *testing.T) {
	blocks := homeBlocks(t, Home{Version: "v1", Admin: true})
	if got := strings.Join(blockTypes(blocks), ","); got != "image,section" {
		t.Fatalf("blocks = %s", got)
	}
}

// Everything that operates Riggs is the admin's alone.
func TestTheJobRowsAreAdminOnly(t *testing.T) {
	blocks := homeBlocks(t, Home{Version: "v1", ShowJobs: true, Jobs: jobRows()})
	if got := strings.Join(blockTypes(blocks), ","); got != "image,section" {
		t.Fatalf("blocks = %s, want the portrait and the version alone", got)
	}
}

// A paused job keeps every other control: it is paused, not broken.
func TestTheMenuOffersEnableOrDisable(t *testing.T) {
	blocks := homeBlocks(t, Home{Version: "v1", Admin: true, ShowJobs: true, Jobs: jobRows()})

	enabled := jobOptions(t, blocks[4])
	if len(enabled) != 4 {
		t.Fatalf("options = %v", enabled)
	}
	if enabled[2].label != MarkerWarning+"  Disable" {
		t.Errorf("an enabled job offers %q", enabled[2].label)
	}
	disabled := jobOptions(t, blocks[5])
	if disabled[2].label != MarkerDone+"  Enable" {
		t.Errorf("a disabled job offers %q", disabled[2].label)
	}
	if disabled[2].value != HomeJobToggleIntent {
		t.Errorf("both spellings must carry the same intent: %q", disabled[2].value)
	}
}

// The one control on this surface that destroys something. An overflow gives no
// second chance of its own, and "I meant to press Disable" is one row away.
// No option carries a `confirm`, and that is the whole point.
//
// Slack's confirmation dialog belongs to the interactive element, not to an
// option inside it. An option carrying one is an invalid block, and one invalid
// block fails the entire view — which is how the Jobs section took the Home tab
// down the first time a job existed. The second chance is JobDeleteModal now.
func TestNoOptionCarriesAConfirm(t *testing.T) {
	blocks := homeBlocks(t, Home{Version: "v1", Admin: true, ShowJobs: true, Jobs: jobRows()})
	options := blocks[4]["accessory"].(map[string]any)["options"].([]any)

	del := options[3].(map[string]any)
	if del["value"] != HomeJobDeleteIntent {
		t.Fatalf("the last option is %v", del["value"])
	}
	for i, opt := range options {
		if _, found := opt.(map[string]any)["confirm"]; found {
			t.Errorf("option %d carries a confirm; Slack rejects the whole view for it", i)
		}
	}
}

// The confirmation still names the job and still points at the gentler option —
// it just does it in a modal, where it can be asked about one option instead of
// all four.
func TestTheDeleteModalAsksAboutOneJob(t *testing.T) {
	view := encodeView(t, JobDeleteModal{Name: "github-review-queue"}.View())

	if view["callback_id"] != JobDeleteModalCallbackID {
		t.Errorf("callback_id = %v", view["callback_id"])
	}
	// The name rides in private_metadata: the submission acts on this, not on
	// whatever the Home tab happens to say by then.
	if view["private_metadata"] != "github-review-queue" {
		t.Errorf("private_metadata = %v", view["private_metadata"])
	}
	if submit := text(view["submit"]); submit != "Delete" {
		t.Errorf("submit = %q, want the button to say what it does", submit)
	}
	if close := text(view["close"]); close != "Keep it" {
		t.Errorf("close = %q", close)
	}

	blocks := view["blocks"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want the one question", len(blocks))
	}
	body := text(blocks[0].(map[string]any)["text"])
	if !strings.Contains(body, "github-review-queue") || !strings.Contains(body, "Disable") {
		t.Errorf("question = %q, want the job named and Disable offered", body)
	}
}

// An empty name still renders. A rejected view is a click that does nothing and
// explains nothing, which is worse than a vague noun.
func TestTheDeleteModalSurvivesAnEmptyName(t *testing.T) {
	view := encodeView(t, JobDeleteModal{}.View())
	body := text(view["blocks"].([]any)[0].(map[string]any)["text"])
	if strings.TrimSpace(body) == "" || strings.HasPrefix(body, "**") {
		t.Errorf("question = %q, want a readable fallback", body)
	}
}

// text pulls the string out of a Slack text object.
func text(obj any) string {
	m, ok := obj.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["text"].(string)
	return s
}

// encodeView round-trips a view through JSON, so the assertions are on the
// bytes Slack receives rather than on the Go structs.
func encodeView(t *testing.T, view any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshalling the view: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding the view: %v", err)
	}
	return out
}

// jobOption is one menu entry, flattened.
type jobOption struct{ label, value string }

func jobOptions(t *testing.T, block map[string]any) []jobOption {
	t.Helper()
	acc, ok := block["accessory"].(map[string]any)
	if !ok {
		t.Fatalf("no menu on %v", block)
	}
	if acc["action_id"] != HomeJobActionID {
		t.Fatalf("action_id = %v", acc["action_id"])
	}
	raw := acc["options"].([]any)
	out := make([]jobOption, 0, len(raw))
	for _, o := range raw {
		opt := o.(map[string]any)
		out = append(out, jobOption{
			label: opt["text"].(map[string]any)["text"].(string),
			value: opt["value"].(string),
		})
	}
	return out
}

// --- the job editors --------------------------------------------------------

// The GitHub form is about the ONE review queue, so it has no name field: the
// job's identity is Riggs' to choose, or already decided.
func TestGitHubJobModalFields(t *testing.T) {
	fresh := modalOf(t, GitHubJobModal{Schedule: "3m"})
	if got, present := fresh["private_metadata"]; present && got != "" {
		t.Fatalf("a job that does not exist yet carries an identity: %v", got)
	}
	blocks := fresh["blocks"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want the checkbox, the login and the frequency", len(blocks))
	}
	ids := []string{
		GitHubJobModalEnabledBlockID,
		GitHubJobModalLoginBlockID,
		GitHubJobModalScheduleBlockID,
	}
	for i, want := range ids {
		if got := blocks[i].(map[string]any)["block_id"]; got != want {
			t.Fatalf("block %d = %v, want %q", i, got, want)
		}
	}

	existing := modalOf(t, GitHubJobModal{
		Name: "pull-requests-reviewer", Login: "miere", Schedule: "5m", Enabled: true})
	if existing["private_metadata"] != "pull-requests-reviewer" {
		t.Fatalf("private_metadata = %v", existing["private_metadata"])
	}
	// Pre-filled, so opening it to change the cadence does not silently forget
	// the login.
	login := existing["blocks"].([]any)[1].(map[string]any)["element"].(map[string]any)
	if login["initial_value"] != "miere" {
		t.Fatalf("initial_value = %v", login["initial_value"])
	}
}

// The checkbox is what creates and destroys the job, so its two states have to
// be expressible. An unticked checkbox group is EMPTY, and Slack refuses to
// submit a required input that is empty — a required one could be ticked and
// never unticked.
func TestTheGitHubCheckboxIsOptional(t *testing.T) {
	blocks := modalOf(t, GitHubJobModal{})["blocks"].([]any)
	enabled := blocks[0].(map[string]any)
	if optional, _ := enabled["optional"].(bool); !optional {
		t.Fatal("the checkbox is required, so it could never be unticked")
	}
	for _, b := range blocks[1:] {
		block := b.(map[string]any)
		if optional, _ := block["optional"].(bool); optional {
			t.Errorf("%v is optional and should not be", block["block_id"])
		}
	}
}

// `initial_options` must be ABSENT when nothing is ticked. An empty array is
// not "none selected" to Slack — it is an invalid element, and the modal simply
// does not open.
func TestTheGitHubCheckboxOmitsAnEmptyInitialOptions(t *testing.T) {
	off := modalOf(t, GitHubJobModal{Enabled: false})["blocks"].([]any)
	element := off[0].(map[string]any)["element"].(map[string]any)
	if got, present := element["initial_options"]; present {
		t.Fatalf("initial_options = %v on an unticked box, want it omitted", got)
	}
	if element["type"] != "checkboxes" || element["action_id"] != JobModalActionID {
		t.Fatalf("element = %v", element)
	}

	on := modalOf(t, GitHubJobModal{Enabled: true})["blocks"].([]any)
	initial := on[0].(map[string]any)["element"].(map[string]any)["initial_options"].([]any)
	if len(initial) != 1 || initial[0].(map[string]any)["value"] != GitHubJobModalEnabledValue {
		t.Fatalf("initial_options = %v", initial)
	}
}

// Unticking deletes, and Disable pauses. The two are one click apart on the
// same surface, so the destructive one says what it does at the moment of
// ticking rather than afterwards.
func TestTheGitHubCheckboxSaysThatUntickingDeletes(t *testing.T) {
	blocks := modalOf(t, GitHubJobModal{})["blocks"].([]any)
	option := blocks[0].(map[string]any)["element"].(map[string]any)["options"].([]any)[0]
	description := text(option.(map[string]any)["description"])
	for _, want := range []string{"DELETES", "Disable"} {
		if !strings.Contains(description, want) {
			t.Errorf("description = %q, want it to mention %q", description, want)
		}
	}
}

// A new job has no identity yet, so the name is a field. An existing one's is
// not: it is what the ledger keys on and what the row's block_id carries.
func TestJiraJobModalAsksForANameOnlyWhenCreating(t *testing.T) {
	fresh := modalOf(t, JiraJobModal{Schedule: "3m"})
	if got, present := fresh["private_metadata"]; present && got != "" {
		t.Fatalf("a new job carries an identity: %v", got)
	}
	if fresh["title"].(map[string]any)["text"] != "New Jira job" {
		t.Fatalf("title = %v", fresh["title"])
	}
	if got := len(fresh["blocks"].([]any)); got != 3 {
		t.Fatalf("blocks = %d, want name, JQL and frequency", got)
	}

	existing := modalOf(t, JiraJobModal{
		Name: "tickets", JQL: `project = NYX AND status = "Ready"`, Schedule: "3m"})
	if existing["private_metadata"] != "tickets" {
		t.Fatalf("private_metadata = %v", existing["private_metadata"])
	}
	blocks := existing["blocks"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want no name field when editing", len(blocks))
	}
	if blocks[0].(map[string]any)["block_id"] != JiraJobModalJQLBlockID {
		t.Fatalf("first block = %v", blocks[0])
	}
	element := blocks[0].(map[string]any)["element"].(map[string]any)
	if element["initial_value"] != `project = NYX AND status = "Ready"` {
		t.Fatalf("initial_value = %v", element["initial_value"])
	}
}

// Every field on either job form is required. A digest with no query advertises
// nothing and one with no schedule runs never, and Slack refusing an empty box
// beats a handler explaining it after the modal has closed.
func TestEveryJiraJobFieldIsRequired(t *testing.T) {
	for _, b := range modalOf(t, JiraJobModal{})["blocks"].([]any) {
		block := b.(map[string]any)
		if optional, _ := block["optional"].(bool); optional {
			t.Errorf("%v is optional and should not be", block["block_id"])
		}
	}
}

// The JQL box is the one multiline job field, and alone in that. A real query
// runs to several clauses, and a single-line input shows about forty characters
// of it — which is how somebody edits the wrong half of their own filter.
func TestOnlyTheJQLIsMultiline(t *testing.T) {
	for _, b := range modalOf(t, JiraJobModal{})["blocks"].([]any) {
		block := b.(map[string]any)
		element := block["element"].(map[string]any)
		multiline, _ := element["multiline"].(bool)
		if block["block_id"] == JiraJobModalJQLBlockID {
			if !multiline {
				t.Error("the JQL field is single-line")
			}
			continue
		}
		if multiline {
			t.Errorf("%v is multiline", block["block_id"])
		}
	}
	for _, b := range modalOf(t, GitHubJobModal{})["blocks"].([]any)[1:] {
		element := b.(map[string]any)["element"].(map[string]any)
		if multiline, _ := element["multiline"].(bool); multiline {
			t.Errorf("%v is multiline", b.(map[string]any)["block_id"])
		}
	}
}

// Two forms, two callback_ids. A router matching them apart is what keeps a
// submission of one from ever being read as the other — one of them carries a
// checkbox that deletes a job.
func TestTheTwoJobEditorsAreToldApart(t *testing.T) {
	github := modalOf(t, GitHubJobModal{})["callback_id"]
	jira := modalOf(t, JiraJobModal{})["callback_id"]
	if github == jira {
		t.Fatalf("both editors submit under %v", github)
	}
	if github != GitHubJobModalCallbackID || jira != JiraJobModalCallbackID {
		t.Fatalf("callback ids = %v / %v", github, jira)
	}
}
