package apphome

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/config"
)

// Configuration: how Riggs' jobs behave, edited from the Home tab.
//
// One setting per KIND of job — how long a run of it may take — behind one menu
// option, for the reason Customisation's four emojis are behind theirs: these
// are set once and revisited on the day something starts timing out, and a row
// each above the schedule would push what an admin reads daily below the fold
// to make room for what they read annually.
//
// It is a separate surface from Customisation rather than two more fields on
// it. The two are read by different people at different moments: one is how
// Riggs looks, the other is how it behaves when nobody is watching. See the
// modal's own comment for the rest of that argument.
//
// The admin gate is the same as everywhere else here, and re-checked in every
// handler for the same reason: the menu is only ever rendered for the admin,
// but an action_id and a callback_id are just strings in a payload, and these
// write to the config file.

// ConfigurationStore is the job settings the modal reads and writes.
//
// *config.Config satisfies it. It is an interface so this package can be tested
// without a config file on disk — the YAML surgery is config's business and
// config's tests are where it is proved.
//
// The list of timeouts is deliberately NOT on here. It is a constant table
// (config.JobTimeoutSpecs), not state, and putting it behind a seam would
// invite a fake that disagrees with the real one about which kinds exist — the
// same call PromptStore and CustomisationStore made.
type ConfigurationStore interface {
	// JobTimeoutText is the bound in force for one kind, as it should appear in
	// the box: configured, or the default.
	JobTimeoutText(id config.JobTimeoutID) string
	// SetJobTimeout rewrites one. An empty value resets it to the default.
	SetJobTimeout(id config.JobTimeoutID, raw string) error
}

// Configure opens the Configuration editor.
//
// It opens the modal and does nothing else first, on the rule EditPrompt
// states: a trigger id lives about three seconds, so anything done before
// views.open is time spent on a modal that will not open — and the only symptom
// is Slack reporting "expired_trigger_id" to a log nobody is reading.
func (p *Publisher) Configure(ctx context.Context, userID, triggerID string) error {
	if !p.IsAdmin(userID) {
		p.deps.Logger.Warn("denied a configuration from a non-admin", "user", userID)
		return fmt.Errorf("apphome: %s is not the admin", userID)
	}
	if p.deps.Modals == nil || p.deps.Configuration == nil {
		return fmt.Errorf("apphome: configuration is not wired up in this build")
	}

	specs := config.JobTimeoutSpecs()
	fields := make([]blockkit.ConfigurationTimeout, 0, len(specs))
	for _, spec := range specs {
		fields = append(fields, blockkit.ConfigurationTimeout{
			ID:    string(spec.ID),
			Label: spec.Label,
			Hint:  spec.Hint,
			// The EFFECTIVE bound, not the raw configured one. An admin opening
			// this to give a digest more room wants to see what it has now, and
			// "empty means two minutes" is a distinction the writer needs and
			// the reader does not.
			Value: p.deps.Configuration.JobTimeoutText(spec.ID),
		})
	}
	return p.deps.Modals.OpenView(ctx, p.deps.BotToken, triggerID,
		blockkit.ConfigurationModal{Timeouts: fields}.View())
}

// SaveConfiguration records a submitted form and redraws the tab.
//
// timeouts maps a kind's token to the duration typed for it. It comes from the
// daemon, which read the values out of the submission by (block_id, action_id)
// — this signature is deliberately made of plain values rather than a Slack
// callback, so the rules below can be tested without one.
//
// EVERY field is written, including the ones that did not change, on
// SaveCustomisation's reasoning: a value that happens to equal today's DEFAULT
// would be stored as an override under one reading and left unset under the
// other, and the two behave differently the day the default changes. Writing
// what the form says keeps the file a record of what the admin was looking at.
//
// One failure does not abandon the rest. A duration that will not parse is one
// field's problem, and refusing the other kind's change because of it would
// make a form of two settings only as usable as its worse entry.
func (p *Publisher) SaveConfiguration(ctx context.Context, userID string, timeouts map[string]string) error {
	if !p.IsAdmin(userID) {
		p.deps.Logger.Warn("denied a configuration save from a non-admin", "user", userID)
		return fmt.Errorf("apphome: %s is not the admin", userID)
	}
	if p.deps.Configuration == nil {
		return fmt.Errorf("apphome: configuration is not wired up in this build")
	}

	var problems []error
	for _, spec := range config.JobTimeoutSpecs() {
		typed, present := timeouts[string(spec.ID)]
		if !present {
			// A field this build renders that the submission did not carry: a
			// modal opened before an update and submitted after it. Leaving the
			// setting alone is the only safe reading — the admin never saw a box
			// for it.
			continue
		}
		typed = strings.TrimSpace(typed)
		// A bound equal to the built-in is stored as a reset rather than as an
		// override, so a later change to the default reaches this machine (the
		// rule SetPrompt and SetReaction both follow). The admin sees no
		// difference; the file stops carrying a setting that says nothing.
		if typed == spec.Default.String() {
			typed = ""
		}
		if err := p.deps.Configuration.SetJobTimeout(spec.ID, typed); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", spec.Label, err))
		}
	}

	// Redrawn before the outcome is reported, and whatever it was. The admin has
	// just pressed Save and is looking at the surface the change lands on — and
	// after a PARTIAL failure the redrawn tab is the only honest picture of what
	// did take.
	p.republish(ctx, userID)

	if len(problems) > 0 {
		err := errors.Join(problems...)
		p.deps.Logger.Error("could not save every job setting", "user", userID, "error", err)
		return err
	}
	p.deps.Logger.Info("configuration saved", "user", userID)
	return nil
}
