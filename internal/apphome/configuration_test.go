package apphome

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/config"
)

// jobSettings is an in-memory ConfigurationStore. The YAML surgery is config's
// business and config's tests prove it; what matters here is WHICH writes this
// package asks for — the settings idiom customise_test.go established.
type jobSettings struct {
	timeout map[config.JobTimeoutID]string
	writes  []string
	err     map[config.JobTimeoutID]error
}

func newJobSettings() *jobSettings {
	return &jobSettings{
		timeout: map[config.JobTimeoutID]string{},
		err:     map[config.JobTimeoutID]error{},
	}
}

func (s *jobSettings) JobTimeoutText(id config.JobTimeoutID) string {
	if v := s.timeout[id]; v != "" {
		return v
	}
	spec, _ := config.LookupJobTimeout(id)
	return spec.Default.String()
}

func (s *jobSettings) SetJobTimeout(id config.JobTimeoutID, raw string) error {
	if err := s.err[id]; err != nil {
		return err
	}
	s.writes = append(s.writes, string(id)+"="+raw)
	s.timeout[id] = raw
	return nil
}

// configRig assembles a publisher with the Configuration surface wired.
type configRig struct {
	*Publisher
	views  *fakeViews
	modals *fakeModals
	store  *jobSettings
}

func newConfigRig(t *testing.T) *configRig {
	t.Helper()
	r := &configRig{views: &fakeViews{}, modals: &fakeModals{}, store: newJobSettings()}
	r.Publisher = New(Deps{
		Version: "v1.0.0", BotToken: "xoxb", AdminUserID: admin,
		Views: r.views, Modals: r.modals, Configuration: r.store,
		Restart: func(context.Context) error { return nil },
		Logger:  quiet(),
	})
	return r
}

// Every field is pre-filled with the bound actually in force, not with the raw
// setting. An admin opening this to give a digest more room wants to see what
// it has now.
func TestConfigurationOpensPrefilledWithWhatIsRunning(t *testing.T) {
	r := newConfigRig(t)
	r.store.timeout[config.JobTimeoutJira] = "10m"

	if err := r.Configure(context.Background(), admin, "trigger-1"); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if r.modals.triggerID != "trigger-1" {
		t.Errorf("trigger = %q", r.modals.triggerID)
	}
	if r.modals.view["callback_id"] != blockkit.ConfigurationModalCallbackID {
		t.Fatalf("callback = %v", r.modals.view["callback_id"])
	}
	jira := modalField(t, r.modals.view,
		blockkit.ConfigurationTimeoutBlockPrefix+string(config.JobTimeoutJira))
	if jira != "10m" {
		t.Fatalf("jira pre-fill = %q", jira)
	}
	// And the kind nobody has configured shows its DEFAULT rather than an empty
	// box: "empty means two minutes" is a distinction the writer needs and the
	// reader does not.
	github := modalField(t, r.modals.view,
		blockkit.ConfigurationTimeoutBlockPrefix+string(config.JobTimeoutGitHub))
	if github != config.DefaultJobTimeout.String() {
		t.Fatalf("github pre-fill = %q, want the default", github)
	}
}

// Every field is written, including the ones that did not change: a value that
// happens to equal today's default would be stored as an override under one
// reading and left unset under the other, and the two behave differently the
// day the default changes.
func TestSaveConfigurationWritesEveryField(t *testing.T) {
	r := newConfigRig(t)

	err := r.SaveConfiguration(context.Background(), admin, map[string]string{
		string(config.JobTimeoutGitHub): "5m",
		string(config.JobTimeoutJira):   "10m",
	})
	if err != nil {
		t.Fatalf("SaveConfiguration: %v", err)
	}
	if len(r.store.writes) != 2 {
		t.Fatalf("writes = %v, want one per kind", r.store.writes)
	}
	if r.store.timeout[config.JobTimeoutGitHub] != "5m" {
		t.Fatalf("github = %q", r.store.timeout[config.JobTimeoutGitHub])
	}
	// And the tab is redrawn: the admin has just pressed Save and is looking at
	// the surface the change lands on.
	if r.views.count() == 0 {
		t.Fatal("the tab was not republished")
	}
}

// A bound equal to the built-in is stored as a RESET rather than an override,
// so a later change to the default reaches this machine.
func TestATimeoutEqualToTheDefaultIsStoredAsAReset(t *testing.T) {
	r := newConfigRig(t)

	err := r.SaveConfiguration(context.Background(), admin, map[string]string{
		string(config.JobTimeoutJira): config.DefaultJobTimeout.String(),
	})
	if err != nil {
		t.Fatalf("SaveConfiguration: %v", err)
	}
	want := string(config.JobTimeoutJira) + "="
	if len(r.store.writes) != 1 || r.store.writes[0] != want {
		t.Fatalf("writes = %v, want %q", r.store.writes, want)
	}
}

// A field this build renders that the submission did not carry is a modal
// opened before an update and submitted after it. Leaving the setting alone is
// the only safe reading — the admin never saw a box for it.
func TestAMissingTimeoutFieldIsLeftAlone(t *testing.T) {
	r := newConfigRig(t)
	r.store.timeout[config.JobTimeoutJira] = "10m"

	err := r.SaveConfiguration(context.Background(), admin, map[string]string{
		string(config.JobTimeoutGitHub): "5m",
	})
	if err != nil {
		t.Fatalf("SaveConfiguration: %v", err)
	}
	if r.store.timeout[config.JobTimeoutJira] != "10m" {
		t.Fatalf("jira = %q, want it untouched", r.store.timeout[config.JobTimeoutJira])
	}
}

// One failure does not abandon the rest. A duration that will not parse is one
// field's problem, and refusing the other kind's change because of it would
// make a form of two settings only as usable as its worse entry.
func TestOneBadTimeoutDoesNotBlockTheOther(t *testing.T) {
	r := newConfigRig(t)
	r.store.err[config.JobTimeoutJira] = errors.New(`"soon" is not a duration`)

	err := r.SaveConfiguration(context.Background(), admin, map[string]string{
		string(config.JobTimeoutGitHub): "5m",
		string(config.JobTimeoutJira):   "soon",
	})
	if err == nil {
		t.Fatal("a rejected timeout reported success")
	}
	if !strings.Contains(err.Error(), "not a duration") {
		t.Fatalf("err = %v, want the reason", err)
	}
	if r.store.timeout[config.JobTimeoutGitHub] != "5m" {
		t.Fatal("the good field was abandoned because of the bad one")
	}
	// Redrawn anyway: after a PARTIAL failure the tab is the only honest
	// picture of what did take.
	if r.views.count() == 0 {
		t.Fatal("the tab was not republished after a partial failure")
	}
}

// The menu is only ever rendered for the admin, but a callback_id is just a
// string in a payload and these two write to the config file.
func TestConfigurationIsAdminOnly(t *testing.T) {
	r := newConfigRig(t)
	ctx, someone := context.Background(), "U-someone"

	if err := r.Configure(ctx, someone, "trigger-1"); err == nil {
		t.Error("a non-admin opened the Configuration modal")
	}
	if r.modals.view != nil {
		t.Error("a modal was opened for a non-admin")
	}
	err := r.SaveConfiguration(ctx, someone, map[string]string{
		string(config.JobTimeoutJira): "10m",
	})
	if err == nil {
		t.Error("a non-admin saved a job setting")
	}
	if len(r.store.writes) != 0 {
		t.Errorf("writes = %v, want none", r.store.writes)
	}
}

// A build with no settings store draws no Configuration option, on the rule the
// rest of this surface obeys: a control that cannot act is not drawn.
func TestNoStoreMeansNoConfigurationOption(t *testing.T) {
	r := newConfigRig(t)
	if !r.render(context.Background(), admin).ShowConfiguration {
		t.Fatal("the option is missing with a store wired")
	}
	r.deps.Configuration = nil
	if r.render(context.Background(), admin).ShowConfiguration {
		t.Fatal("the option is drawn with no store behind it")
	}
}
