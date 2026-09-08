package apphome

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/config"
)

// Customisation: how Riggs presents itself, edited from the Home tab.
//
// Two kinds of setting behind one menu option — the four reaction emojis
// (§7f) and the banner — because neither earns a row of its own. A row is for
// something you come back to; these are set once and forgotten, and four emoji
// rows above the jobs would push what an admin reads daily below the fold to
// make room for what they read annually.
//
// The admin gate is the same as everywhere else on this surface, and re-checked
// in every handler for the same reason: the menu is only ever rendered for the
// admin, but an action_id and a callback_id are just strings in a payload, and
// these two write to the config file.

// CustomisationStore is the presentation settings the modal reads and writes.
//
// *config.Config satisfies it. It is an interface so this package can be tested
// without a config file on disk — the YAML surgery is config's business and
// config's tests are where it is proved.
//
// The list of reaction states is deliberately NOT on here. It is a constant
// table (config.ReactionSpecs), not state, and putting it behind a seam would
// invite a fake that disagrees with the real one about which states exist —
// the same call PromptStore made.
type CustomisationStore interface {
	// ReactionEmoji is the shortcode in force for one state: configured, or the
	// built-in default.
	ReactionEmoji(id config.ReactionID) string
	// SetReaction rewrites one. An empty name resets it to the default.
	SetReaction(id config.ReactionID, name string) error
	// ShowBanner reports whether the Home tab draws the portrait.
	ShowBanner() bool
	// SetShowBanner records the choice.
	SetShowBanner(show bool) error
}

// Customise opens the Customisation editor.
//
// It opens the modal and does nothing else first, on the rule EditPrompt
// states: a trigger id lives about three seconds, so anything done before
// views.open is time spent on a modal that will not open, and the only symptom
// is Slack reporting "expired_trigger_id" to a log nobody is reading.
func (p *Publisher) Customise(ctx context.Context, userID, triggerID string) error {
	if !p.IsAdmin(userID) {
		p.deps.Logger.Warn("denied a customisation from a non-admin", "user", userID)
		return fmt.Errorf("apphome: %s is not the admin", userID)
	}
	if p.deps.Modals == nil || p.deps.Customisation == nil {
		return fmt.Errorf("apphome: customisation is not configured")
	}

	specs := config.ReactionSpecs()
	fields := make([]blockkit.CustomisationEmoji, 0, len(specs))
	for _, spec := range specs {
		fields = append(fields, blockkit.CustomisationEmoji{
			ID:    string(spec.ID),
			Label: spec.Label,
			Hint:  spec.Hint,
			// The EFFECTIVE emoji, not the raw configured one. An admin opening
			// this to change the tick wants to see which tick is on their
			// messages, and "empty means the default" is a distinction the
			// writer needs and the reader does not.
			Value: p.deps.Customisation.ReactionEmoji(spec.ID),
		})
	}
	return p.deps.Modals.OpenView(ctx, p.deps.BotToken, triggerID, blockkit.CustomisationModal{
		Emojis:     fields,
		ShowBanner: p.deps.Customisation.ShowBanner(),
	}.View())
}

// SaveCustomisation records a submitted form and redraws the tab.
//
// emojis maps a state's token to the shortcode typed for it; banner is the
// select's chosen value. Both come from the daemon, which read them out of the
// submission by (block_id, action_id) — this signature is deliberately made of
// plain values rather than a Slack callback, so the rules below can be tested
// without one.
//
// EVERY field is written, including the ones that did not change. The
// alternative is comparing each against what is in force and skipping the
// matches, which sounds tidier and is wrong in a specific way: an emoji whose
// typed value happens to equal today's DEFAULT would be written as an override
// under one reading and left unset under the other, and the two behave
// differently the day the default changes. Writing what the form says keeps the
// file a record of what the admin was looking at when they pressed Save.
//
// One failure does not abandon the rest. A rejected emoji name is one field's
// problem, and refusing the banner change because of it would make a form of
// five settings only as usable as its worst entry.
func (p *Publisher) SaveCustomisation(ctx context.Context, userID string,
	emojis map[string]string, banner string) error {

	if !p.IsAdmin(userID) {
		p.deps.Logger.Warn("denied a customisation save from a non-admin", "user", userID)
		return fmt.Errorf("apphome: %s is not the admin", userID)
	}
	if p.deps.Customisation == nil {
		return fmt.Errorf("apphome: customisation is not configured")
	}

	var problems []error
	for _, spec := range config.ReactionSpecs() {
		typed, present := emojis[string(spec.ID)]
		if !present {
			// A field this build renders that the submission did not carry:
			// a modal opened before an update and submitted after it. Leaving
			// the setting alone is the only safe reading — the admin never saw
			// a box for it.
			continue
		}
		typed = strings.TrimSpace(typed)
		// An emoji that matches the built-in is stored as a reset rather than
		// as an override, so a later change to the default reaches this machine
		// (the rule SetPrompt follows). The admin sees no difference; the file
		// stops carrying a setting that says nothing.
		if typed == spec.Default {
			typed = ""
		}
		if err := p.deps.Customisation.SetReaction(spec.ID, typed); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", spec.Label, err))
		}
	}

	show := banner != blockkit.CustomisationBannerHide
	if err := p.deps.Customisation.SetShowBanner(show); err != nil {
		problems = append(problems, fmt.Errorf("banner: %w", err))
	}

	// Redrawn before the outcome is reported, and whatever it was. The admin
	// has just pressed Save and is looking at the surface the change lands on;
	// a tab still showing the old banner reads as a Save that did not take —
	// and after a PARTIAL failure it is the only honest picture of what did.
	p.republish(ctx, userID)

	if len(problems) > 0 {
		err := errors.Join(problems...)
		p.deps.Logger.Error("could not save every customisation", "user", userID, "error", err)
		return err
	}
	p.deps.Logger.Info("customisation saved", "user", userID, "banner", show)
	return nil
}
