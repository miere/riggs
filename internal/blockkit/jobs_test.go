package blockkit

import (
	"strings"
	"testing"
)

func jobRows() []HomeJob {
	return []HomeJob{
		{ID: "github-review-queue", Schedule: "3m", Command: "git pr --bulk miere",
			Status: MarkerDone + " ran 2m ago in 1.4s · next in 58s", Enabled: true},
		{ID: "nightly", Schedule: "0 9 * * 1-5", Command: "jira tickets --bulk",
			Status: MarkerWarning + " disabled", Enabled: false},
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
	for _, want := range []string{"*github-review-queue*", "_3m_", "`git pr --bulk miere`", "ran 2m ago"} {
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
	if text := elements[0].(map[string]any)["text"].(string); !strings.Contains(text, "New job") {
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
func TestDeleteIsConfirmed(t *testing.T) {
	blocks := homeBlocks(t, Home{Version: "v1", Admin: true, ShowJobs: true, Jobs: jobRows()})
	options := blocks[4]["accessory"].(map[string]any)["options"].([]any)

	del := options[3].(map[string]any)
	if del["value"] != HomeJobDeleteIntent {
		t.Fatalf("the last option is %v", del["value"])
	}
	confirm, ok := del["confirm"].(map[string]any)
	if !ok {
		t.Fatal("Delete has no confirmation")
	}
	if confirm["style"] != "danger" {
		t.Errorf("confirm style = %v", confirm["style"])
	}
	// It names the job, and says what the gentler option would do instead.
	text := confirm["text"].(map[string]any)["text"].(string)
	if !strings.Contains(text, "github-review-queue") || !strings.Contains(text, "Disable") {
		t.Errorf("confirmation = %q", text)
	}

	// And nothing else carries one: a control that asks twice is one people
	// learn to click through.
	for i, opt := range options {
		if i == 3 {
			continue
		}
		if _, found := opt.(map[string]any)["confirm"]; found {
			t.Errorf("option %d is confirmed and should not be", i)
		}
	}
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

// A new job has no identity yet, so the name is a field. An existing one's is
// not: it is what the ledger keys on and what the row's block_id carries.
func TestJobModalAsksForANameOnlyWhenCreating(t *testing.T) {
	fresh := modalOf(t, JobModal{Schedule: "3m"})
	// Absent, not empty: a new job has no identity, and the submission reads
	// the missing field back as the empty string either way.
	if got, present := fresh["private_metadata"]; present && got != "" {
		t.Fatalf("a new job carries an identity: %v", got)
	}
	if fresh["title"].(map[string]any)["text"] != "New job" {
		t.Fatalf("title = %v", fresh["title"])
	}
	if got := len(fresh["blocks"].([]any)); got != 4 {
		t.Fatalf("blocks = %d, want name, command, schedule and timeout", got)
	}

	existing := modalOf(t, JobModal{Name: "github-review-queue",
		Command: "git pr --bulk miere", Schedule: "3m", Timeout: "2m"})
	if existing["private_metadata"] != "github-review-queue" {
		t.Fatalf("private_metadata = %v", existing["private_metadata"])
	}
	blocks := existing["blocks"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want no name field when editing", len(blocks))
	}
	if blocks[0].(map[string]any)["block_id"] != JobModalCommandBlockID {
		t.Fatalf("first block = %v", blocks[0])
	}
	// Pre-filled, so an edit starts from what is actually scheduled.
	element := blocks[0].(map[string]any)["element"].(map[string]any)
	if element["initial_value"] != "git pr --bulk miere" {
		t.Fatalf("initial_value = %v", element["initial_value"])
	}
}

// A job with no command runs nothing and a job with no schedule runs never;
// Slack refusing an empty box beats a handler explaining it afterwards.
func TestOnlyTheTimeoutIsOptional(t *testing.T) {
	blocks := modalOf(t, JobModal{Name: "x"})["blocks"].([]any)
	for _, b := range blocks {
		block := b.(map[string]any)
		optional, _ := block["optional"].(bool)
		if block["block_id"] == JobModalTimeoutBlockID {
			if !optional {
				t.Error("the timeout is required")
			}
			continue
		}
		if optional {
			t.Errorf("%v is optional and should not be", block["block_id"])
		}
	}
}

// Single-line inputs: every one of these is a name, a command or a duration,
// and a newline is a value none of them can carry.
func TestTheJobFieldsAreSingleLine(t *testing.T) {
	for _, b := range modalOf(t, JobModal{})["blocks"].([]any) {
		element := b.(map[string]any)["element"].(map[string]any)
		if multiline, _ := element["multiline"].(bool); multiline {
			t.Errorf("%v is multiline", b.(map[string]any)["block_id"])
		}
	}
}
