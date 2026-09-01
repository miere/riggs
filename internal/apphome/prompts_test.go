package apphome

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/config"
)

// --- fakes ------------------------------------------------------------------

// fakePrompts is the wording store. Writing one is config's own business and
// its tests are where the YAML surgery is proved; this only has to remember.
type fakePrompts struct {
	mu       sync.Mutex
	override map[config.PromptID]string
	err      error
}

func newPrompts() *fakePrompts {
	return &fakePrompts{override: map[config.PromptID]string{}}
}

func (f *fakePrompts) PromptText(id config.PromptID) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.override[id]; ok {
		return v
	}
	spec, _ := config.LookupPrompt(id)
	return spec.Default
}

func (f *fakePrompts) PromptOverridden(id config.PromptID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.override[id]
	return ok
}

func (f *fakePrompts) SetPrompt(id config.PromptID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if text == "" {
		delete(f.override, id)
		return nil
	}
	f.override[id] = text
	return nil
}

type fakeModals struct {
	mu        sync.Mutex
	triggerID string
	view      map[string]any
	err       error
}

func (f *fakeModals) OpenView(_ context.Context, _, triggerID string, view any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	raw, err := json.Marshal(view)
	if err != nil {
		return err
	}
	f.triggerID = triggerID
	return json.Unmarshal(raw, &f.view)
}

// promptRig assembles a publisher with the prompt surface wired.
type promptRig struct {
	*Publisher
	views   *fakeViews
	modals  *fakeModals
	prompts *fakePrompts
}

func newPromptRig(t *testing.T) *promptRig {
	t.Helper()
	r := &promptRig{views: &fakeViews{}, modals: &fakeModals{}, prompts: newPrompts()}
	r.Publisher = New(Deps{
		Version: "v1.0.0", BotToken: "xoxb", AdminUserID: admin,
		Views: r.views, Modals: r.modals, Prompts: r.prompts,
		Restart: func(context.Context) error { return nil },
		Logger:  quiet(),
	})
	return r
}

// promptRows lists the prompt sections in a published view.
func (p publishedView) promptRows() []map[string]any {
	blocks, _ := p.view["blocks"].([]any)
	var out []map[string]any
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := block["block_id"].(string); strings.HasPrefix(id, blockkit.HomePromptBlockPrefix) {
			out = append(out, block)
		}
	}
	return out
}

// --- rendering ---------------------------------------------------------------

// Every registered prompt gets a row, in the registry's order.
func TestTheAdminSeesEveryPrompt(t *testing.T) {
	r := newPromptRig(t)
	if _, err := r.Publish(context.Background(), admin); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	rows := r.views.last().promptRows()
	specs := config.Prompts()
	if len(rows) != len(specs) {
		t.Fatalf("rows = %d, want %d", len(rows), len(specs))
	}
	for i, spec := range specs {
		want := blockkit.HomePromptBlockPrefix + string(spec.ID)
		if rows[i]["block_id"] != want {
			t.Fatalf("row %d block_id = %v, want %s", i, rows[i]["block_id"], want)
		}
	}
}

// A non-admin gets the portrait and the version, and nothing that operates
// Riggs. The prompts are how Riggs talks to other people; they are not a
// setting a colleague should be shown, let alone offered.
func TestANonAdminSeesNoPrompts(t *testing.T) {
	r := newPromptRig(t)
	if _, err := r.Publish(context.Background(), "U-someone"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if rows := r.views.last().promptRows(); len(rows) != 0 {
		t.Fatalf("a non-admin was shown %d prompt rows", len(rows))
	}
}

// A control that cannot act is worse than one that was never there — the same
// rule that keeps the Update button off a build with no installer.
func TestNoEditorMeansNoPromptRows(t *testing.T) {
	r := newPromptRig(t)
	r.deps.Modals = nil
	if _, err := r.Publish(context.Background(), admin); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if rows := r.views.last().promptRows(); len(rows) != 0 {
		t.Fatalf("Edit was offered with nothing to open it: %d rows", len(rows))
	}
}

// --- editing -----------------------------------------------------------------

func TestEditPromptOpensTheModalForThatPrompt(t *testing.T) {
	r := newPromptRig(t)
	if err := r.prompts.SetPrompt(config.PromptAIReview, "my wording"); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if err := r.EditPrompt(context.Background(), admin, string(config.PromptAIReview), "trigger-1"); err != nil {
		t.Fatalf("EditPrompt: %v", err)
	}
	if r.modals.triggerID != "trigger-1" {
		t.Fatalf("trigger = %q", r.modals.triggerID)
	}
	if r.modals.view["private_metadata"] != string(config.PromptAIReview) {
		t.Fatalf("the modal is for %v", r.modals.view["private_metadata"])
	}
	// Pre-filled with what is actually running, not with the default.
	blocks := r.modals.view["blocks"].([]any)
	element := blocks[0].(map[string]any)["element"].(map[string]any)
	if element["initial_value"] != "my wording" {
		t.Fatalf("initial_value = %v, want the wording in force", element["initial_value"])
	}
}

// An action_id and a block_id are just strings in a payload, and this one leads
// to a write.
func TestPromptEditingIsAdminOnly(t *testing.T) {
	r := newPromptRig(t)
	id := string(config.PromptAIReview)

	if err := r.EditPrompt(context.Background(), "U-someone", id, "trigger-1"); err == nil {
		t.Fatal("a non-admin opened the editor")
	}
	if r.modals.view != nil {
		t.Fatal("a modal was opened for a non-admin")
	}
	if err := r.SavePrompt(context.Background(), "U-someone", id, "theirs"); err == nil {
		t.Fatal("a non-admin saved a prompt")
	}
	if err := r.ResetPrompt(context.Background(), "U-someone", id); err == nil {
		t.Fatal("a non-admin reset a prompt")
	}
	if r.prompts.PromptOverridden(config.PromptAIReview) {
		t.Fatal("a non-admin changed the configuration")
	}
}

// A Home tab published by an older build names a prompt this one does not have.
func TestAnUnknownPromptIsRefused(t *testing.T) {
	r := newPromptRig(t)
	if err := r.EditPrompt(context.Background(), admin, "nonsense", "trigger-1"); err == nil {
		t.Fatal("the editor opened for a prompt that does not exist")
	}
	if err := r.SavePrompt(context.Background(), admin, "nonsense", "x"); err == nil {
		t.Fatal("an unknown prompt was saved")
	}
}

// The admin has just pressed Save and is looking at the surface the change
// lands on; a tab still showing the old wording reads as a Save that did not
// take.
func TestSavePromptWritesAndRedraws(t *testing.T) {
	r := newPromptRig(t)
	if _, err := r.Publish(context.Background(), admin); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	before := r.views.count()

	if err := r.SavePrompt(context.Background(), admin, string(config.PromptAIAssist), "scope {key} properly"); err != nil {
		t.Fatalf("SavePrompt: %v", err)
	}
	if got := r.prompts.PromptText(config.PromptAIAssist); got != "scope {key} properly" {
		t.Fatalf("stored = %q", got)
	}
	if r.views.count() != before+1 {
		t.Fatalf("published %d times, want one redraw", r.views.count()-before)
	}
	if !strings.Contains(rowText(t, r, config.PromptAIAssist), "scope {key} properly") {
		t.Fatal("the redrawn tab still shows the old wording")
	}
}

// Reset writes an EMPTY value rather than the default's own words, so a later
// change to the default reaches this machine.
func TestResetPromptClearsTheOverride(t *testing.T) {
	r := newPromptRig(t)
	if err := r.SavePrompt(context.Background(), admin, string(config.PromptAIReview), "mine"); err != nil {
		t.Fatalf("SavePrompt: %v", err)
	}
	if err := r.ResetPrompt(context.Background(), admin, string(config.PromptAIReview)); err != nil {
		t.Fatalf("ResetPrompt: %v", err)
	}
	if r.prompts.PromptOverridden(config.PromptAIReview) {
		t.Fatal("the override survived a reset")
	}
	if got := r.prompts.PromptText(config.PromptAIReview); got != config.DefaultAIReviewPrompt {
		t.Fatalf("text = %q, want the default back", got)
	}
}

// A write that failed must come back as a failure: the daemon reports it to
// whoever clicked, and a silent one leaves them believing it saved.
func TestASaveThatCannotBeWrittenIsReported(t *testing.T) {
	r := newPromptRig(t)
	r.prompts.err = errors.New("read-only file system")

	err := r.SavePrompt(context.Background(), admin, string(config.PromptAIReview), "mine")
	if err == nil {
		t.Fatal("a failed write reported success")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("err = %v, want the cause", err)
	}
}

// rowText is one prompt row's rendered body in the last published view.
func rowText(t *testing.T, r *promptRig, id config.PromptID) string {
	t.Helper()
	for _, row := range r.views.last().promptRows() {
		if row["block_id"] == blockkit.HomePromptBlockPrefix+string(id) {
			return row["text"].(map[string]any)["text"].(string)
		}
	}
	t.Fatalf("no row for %s", id)
	return ""
}
