package apphome

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/config"
)

// settings is an in-memory CustomisationStore. The YAML surgery is config's
// business and config's tests prove it; what matters here is WHICH writes this
// package asks for.
type settings struct {
	emoji  map[config.ReactionID]string
	writes []string
	banner bool
	err    map[config.ReactionID]error
}

func newSettings() *settings {
	return &settings{emoji: map[config.ReactionID]string{}, banner: true,
		err: map[config.ReactionID]error{}}
}

func (s *settings) ReactionEmoji(id config.ReactionID) string {
	if v := s.emoji[id]; v != "" {
		return v
	}
	spec, _ := config.LookupReaction(id)
	return spec.Default
}

func (s *settings) SetReaction(id config.ReactionID, name string) error {
	if err := s.err[id]; err != nil {
		return err
	}
	s.writes = append(s.writes, string(id)+"="+name)
	s.emoji[id] = name
	return nil
}

func (s *settings) ShowBanner() bool { return s.banner }

func (s *settings) SetShowBanner(show bool) error {
	s.banner = show
	s.writes = append(s.writes, "banner="+map[bool]string{true: "show", false: "hide"}[show])
	return nil
}

// customRig assembles a publisher with the Customisation surface wired, on the
// same shape promptRig uses: one fake per seam, all of them inspectable.
type customRig struct {
	*Publisher
	views  *fakeViews
	modals *fakeModals
	store  *settings
}

func newCustomRig(t *testing.T) *customRig {
	t.Helper()
	r := &customRig{views: &fakeViews{}, modals: &fakeModals{}, store: newSettings()}
	r.Publisher = New(Deps{
		Version: "v1.0.0", BotToken: "xoxb", AdminUserID: admin,
		Views: r.views, Modals: r.modals, Customisation: r.store,
		Restart: func(context.Context) error { return nil },
		Logger:  quiet(),
	})
	return r
}

// modalJSON renders the last opened modal, so a test can assert on the payload
// Slack would actually receive rather than on a struct nobody sends.
func (r *customRig) modalJSON(t *testing.T) string {
	t.Helper()
	r.modals.mu.Lock()
	defer r.modals.mu.Unlock()
	raw, err := json.Marshal(r.modals.view)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Every field on the modal is pre-filled with the emoji actually in force, not
// with the raw setting. An admin opening this to change the tick wants to see
// which tick is on their messages.
func TestTheModalOpensPrefilledWithWhatIsRunning(t *testing.T) {
	r := newCustomRig(t)
	r.store.emoji[config.ReactionSuccess] = "tada"

	if err := r.Customise(context.Background(), admin, "trigger-1"); err != nil {
		t.Fatalf("Customise: %v", err)
	}
	if r.modals.triggerID != "trigger-1" {
		t.Errorf("trigger = %q", r.modals.triggerID)
	}
	rendered := r.modalJSON(t)
	if !strings.Contains(rendered, `"initial_value":"tada"`) {
		t.Errorf("the override is not pre-filled:\n%s", rendered)
	}
	if !strings.Contains(rendered, `"initial_value":"`+config.DefaultAcknowledgementEmoji+`"`) {
		t.Errorf("an unset field is not pre-filled with its default:\n%s", rendered)
	}
}

// Every control on this surface re-checks the gate. The menu is only ever
// rendered for the admin, but an action_id and a callback_id are just strings in
// a payload, and both of these write to the config file.
func TestOnlyTheAdminMayCustomise(t *testing.T) {
	r := newCustomRig(t)

	if err := r.Customise(context.Background(), "U-SOMEBODY", "trigger-1"); err == nil {
		t.Error("a non-admin opened the modal")
	}
	if r.modals.view != nil {
		t.Error("a modal was opened for a non-admin")
	}
	if err := r.SaveCustomisation(context.Background(), "U-SOMEBODY",
		map[string]string{"success": "tada"}, blockkit.CustomisationBannerHide); err == nil {
		t.Error("a non-admin saved a customisation")
	}
	if len(r.store.writes) != 0 {
		t.Errorf("a non-admin's save wrote %v", r.store.writes)
	}
}

// An emoji typed to match the built-in is stored as a RESET rather than as an
// override, so a later change to the default reaches this machine. The admin
// sees no difference; the file stops carrying a setting that says nothing.
func TestAnEmojiMatchingTheDefaultIsStoredAsAReset(t *testing.T) {
	r := newCustomRig(t)

	err := r.SaveCustomisation(context.Background(), admin, map[string]string{
		string(config.ReactionSuccess): config.DefaultSuccessEmoji,
		string(config.ReactionWarning): "rotating_light",
	}, blockkit.CustomisationBannerShow)
	if err != nil {
		t.Fatalf("SaveCustomisation: %v", err)
	}
	if !contains(r.store.writes, "success=") {
		t.Errorf("the default-matching emoji was not reset: %v", r.store.writes)
	}
	if !contains(r.store.writes, "warning=rotating_light") {
		t.Errorf("the override was not written: %v", r.store.writes)
	}
}

// A field this build renders that the submission did not carry is a modal
// opened before an update and submitted after it. The admin never saw a box for
// it, so the setting is left exactly as it was rather than reset.
func TestAMissingFieldIsLeftAlone(t *testing.T) {
	r := newCustomRig(t)
	r.store.emoji[config.ReactionSuccess] = "tada"

	err := r.SaveCustomisation(context.Background(), admin,
		map[string]string{string(config.ReactionWarning): "rotating_light"},
		blockkit.CustomisationBannerShow)
	if err != nil {
		t.Fatalf("SaveCustomisation: %v", err)
	}
	if contains(r.store.writes, "success=") {
		t.Errorf("a field the form never carried was written: %v", r.store.writes)
	}
	if r.store.emoji[config.ReactionSuccess] != "tada" {
		t.Errorf("the untouched setting changed to %q", r.store.emoji[config.ReactionSuccess])
	}
}

// One rejected field must not abandon the other four. A form of five settings
// would otherwise be only as usable as its worst entry.
func TestOneBadFieldDoesNotAbandonTheRest(t *testing.T) {
	r := newCustomRig(t)
	r.store.err[config.ReactionSuccess] = errors.New("not an emoji name")

	err := r.SaveCustomisation(context.Background(), admin, map[string]string{
		string(config.ReactionSuccess): "two words",
		string(config.ReactionWarning): "rotating_light",
	}, blockkit.CustomisationBannerHide)

	if err == nil {
		t.Fatal("a rejected field reported success")
	}
	if !strings.Contains(err.Error(), "Success") {
		t.Errorf("err = %v, want it to name the field", err)
	}
	if !contains(r.store.writes, "warning=rotating_light") {
		t.Errorf("the good field was abandoned: %v", r.store.writes)
	}
	if r.store.banner {
		t.Error("the banner change was abandoned")
	}
}

// The banner is a switch: anything that is not "hide" is "show", so a
// submission from a build that spelled the option differently fails safe
// towards the portrait being visible rather than towards a blank tab.
func TestTheBannerSwitchFailsTowardsVisible(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{blockkit.CustomisationBannerShow, true},
		{blockkit.CustomisationBannerHide, false},
		{"", true},
		{"something_else", true},
	} {
		r := newCustomRig(t)
		if err := r.SaveCustomisation(context.Background(), admin, nil, tc.value); err != nil {
			t.Fatalf("SaveCustomisation(%q): %v", tc.value, err)
		}
		if r.store.banner != tc.want {
			t.Errorf("banner after %q = %v, want %v", tc.value, r.store.banner, tc.want)
		}
	}
}

// A save redraws the tab, and does so even when part of it failed: after a
// partial failure the redrawn tab is the only honest picture of what took.
func TestASaveRedrawsTheTab(t *testing.T) {
	r := newCustomRig(t)
	r.store.err[config.ReactionSuccess] = errors.New("nope")

	_ = r.SaveCustomisation(context.Background(), admin,
		map[string]string{string(config.ReactionSuccess): "x"}, blockkit.CustomisationBannerHide)

	if r.views.count() == 0 {
		t.Error("the tab was not redrawn after a save")
	}
}

// A build with no settings store draws no Customisation option and refuses the
// click, rather than opening a modal that saves into nothing.
func TestWithNoStoreCustomisationIsNotOffered(t *testing.T) {
	views, modals := &fakeViews{}, &fakeModals{}
	p := New(Deps{
		Version: "v1.0.0", BotToken: "xoxb", AdminUserID: admin,
		Views: views, Modals: modals,
		Restart: func(context.Context) error { return nil },
		Logger:  quiet(),
	})

	if err := p.Customise(context.Background(), admin, "trigger-1"); err == nil {
		t.Error("the modal opened with nothing behind it")
	}
	if _, err := p.Publish(context.Background(), admin); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	raw, err := json.Marshal(views.last().view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), blockkit.HomeCustomiseIntent) {
		t.Error("the Customisation option was drawn with no store behind it")
	}
}

func contains(haystack []string, prefix string) bool {
	for _, s := range haystack {
		if s == prefix || strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
