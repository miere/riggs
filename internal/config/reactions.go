package config

import (
	"fmt"
	"strings"
	"unicode"
)

// The four emojis Riggs reacts with, named so a surface can list and edit them
// without knowing where in the YAML each one lives.
//
// This is the prompt registry's twin (prompts.go), and deliberately so: the
// Customisation modal edits these the way the prompt editor edits those, and
// two registries with the same shape are cheaper to hold in your head than one
// registry with a mode flag. What differs is the validation — a prompt is prose
// and anything is valid; an emoji is a shortcode Slack either knows or rejects,
// and rejecting it AT THE MODAL is the difference between "that name is not an
// emoji" and a silent `invalid_name` in a log during the next approval.

// ReactionID names one configurable emoji. It is a bare token: it rides in a
// modal's block_id and the router matches it exactly (§7b).
type ReactionID string

const (
	// ReactionAcknowledgement is the emoji for work Riggs has picked up.
	ReactionAcknowledgement ReactionID = "acknowledgement"
	// ReactionDisregard is the emoji for a task Riggs will not tackle.
	ReactionDisregard ReactionID = "disregard"
	// ReactionSuccess is the emoji for a task that finished.
	ReactionSuccess ReactionID = "success"
	// ReactionWarning is the emoji for a task that finished badly.
	ReactionWarning ReactionID = "warning"
)

// The built-in emoji names, as bare shortcodes — no colons, because that is
// what `reactions.add` takes.
//
// They are NAMES rather than literal codepoints on purpose. A literal emoji in
// a Go string is the mistake blockkit/glyphs.go exists to prevent, and it is
// also not what the API wants: `reactions.add` takes `white_check_mark`, not
// the character. Naming them keeps this file inside the source scan's rule
// rather than an exception to it.
const (
	DefaultAcknowledgementEmoji = "saluting_face"
	DefaultDisregardEmoji       = "zipper_mouth_face"
	DefaultSuccessEmoji         = "white_check_mark"
	DefaultWarningEmoji         = "warning"
)

// Reactions is the configured emoji set. Every field is a bare shortcode, and
// an empty one means the default is in force.
type Reactions struct {
	Acknowledgement string `yaml:"acknowledgement"`
	Disregard       string `yaml:"disregard"`
	Success         string `yaml:"success"`
	Warning         string `yaml:"warning"`
}

// ReactionSpec describes one configurable emoji.
type ReactionSpec struct {
	// ID is the token surfaces address it by.
	ID ReactionID
	// Label is the human name, shown in the Customisation modal.
	Label string
	// Hint says when this emoji appears, shown under the modal's input. It is
	// the only place the state machine is explained to the person changing it.
	Hint string
	// Path is where the value lives in the YAML file, as a sequence of mapping
	// keys. The writer creates whatever is missing along it.
	Path []string
	// Default is what an unset emoji resolves to.
	Default string

	// get reads the RAW configured value — empty when the default is in force,
	// which is the distinction the writer needs: resetting deletes the key
	// rather than writing today's default into the file.
	get func(*Config) string
	// set writes the value back into the loaded Config, so the running daemon
	// reacts with an edited emoji without being restarted.
	set func(*Config, string)
}

// reactions is the registry, in the order a task moves through the states:
// picked up, refused, finished, finished badly. A surface listing them in any
// other order would be describing a lifecycle nobody has.
var reactions = []ReactionSpec{
	{
		ID:      ReactionAcknowledgement,
		Label:   "Acknowledgement",
		Hint:    "Riggs has picked the task up. Replaced by the outcome when it finishes.",
		Path:    []string{"reactions", "acknowledgement"},
		Default: DefaultAcknowledgementEmoji,
		get:     func(c *Config) string { return c.Reactions.Acknowledgement },
		set:     func(c *Config, v string) { c.Reactions.Acknowledgement = v },
	},
	{
		ID:      ReactionDisregard,
		Label:   "Disregard",
		Hint:    "Riggs was handed something it does not answer. Final.",
		Path:    []string{"reactions", "disregard"},
		Default: DefaultDisregardEmoji,
		get:     func(c *Config) string { return c.Reactions.Disregard },
		set:     func(c *Config, v string) { c.Reactions.Disregard = v },
	},
	{
		ID:      ReactionSuccess,
		Label:   "Success",
		Hint:    "The task finished. Final.",
		Path:    []string{"reactions", "success"},
		Default: DefaultSuccessEmoji,
		get:     func(c *Config) string { return c.Reactions.Success },
		set:     func(c *Config, v string) { c.Reactions.Success = v },
	},
	{
		ID:      ReactionWarning,
		Label:   "Warning",
		Hint:    "The task failed or finished with a warning; the reason is in the thread. Final.",
		Path:    []string{"reactions", "warning"},
		Default: DefaultWarningEmoji,
		get:     func(c *Config) string { return c.Reactions.Warning },
		set:     func(c *Config, v string) { c.Reactions.Warning = v },
	},
}

// ReactionSpecs lists the configurable emojis, in state order.
//
// A copy, because a caller iterating this to draw a modal has no business
// reaching back through the slice header into the registry.
func ReactionSpecs() []ReactionSpec {
	out := make([]ReactionSpec, len(reactions))
	copy(out, reactions)
	return out
}

// LookupReaction finds one by id. ok is false for a token that names nothing,
// which is what a modal opened by an older build looks like.
func LookupReaction(id ReactionID) (ReactionSpec, bool) {
	for _, s := range reactions {
		if s.ID == id {
			return s, true
		}
	}
	return ReactionSpec{}, false
}

// ReactionEmoji is the effective shortcode for one state: what is configured,
// or the default. An unknown id yields empty rather than panicking — see
// LookupReaction.
func (c *Config) ReactionEmoji(id ReactionID) string {
	spec, ok := LookupReaction(id)
	if !ok {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return effectiveEmoji(c, spec)
}

// effectiveEmoji resolves one spec against an ALREADY-LOCKED config, so the
// whole-set read below can hold one lock rather than taking four.
//
// Not exported and not lock-taking, deliberately: re-entering an RWMutex's read
// lock deadlocks the moment a writer queues between the two acquisitions, and a
// Customisation save is exactly that writer.
func effectiveEmoji(c *Config, spec ReactionSpec) string {
	if v := normaliseEmoji(spec.get(c)); v != "" {
		return v
	}
	return spec.Default
}

// ReactionOverridden reports whether this emoji is configured rather than
// running on its default, so the modal can say so.
func (c *Config) ReactionOverridden(id ReactionID) bool {
	spec, ok := LookupReaction(id)
	if !ok {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return normaliseEmoji(spec.get(c)) != ""
}

// ReactionEmojis is every emoji Riggs may currently have placed, in state
// order.
//
// The state machine needs the whole set, not one member: applying a state means
// adding its emoji and then removing the others, and "the others" is exactly
// this list minus the one going on.
//
// ONE read lock covers the whole loop. Taking four — one per member, which is
// what calling ReactionEmoji in a loop would do — lets a Customisation save land
// between two of them and hand the caller a set that is half old and half new.
// The consequence is small and specific: the removal pass would miss an
// acknowledgement placed under the emoji that just changed, leaving a glyph
// nothing will clear.
func (c *Config) ReactionEmojis() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(reactions))
	for _, spec := range reactions {
		out = append(out, effectiveEmoji(c, spec))
	}
	return out
}

// SetReaction rewrites one emoji in the config file and in this Config.
//
// An empty name RESETS it, on the same rule SetPrompt follows: the key is
// deleted rather than filled with today's default, so "never customised" stays
// distinguishable from "customised to whatever the default said at the time".
func (c *Config) SetReaction(id ReactionID, name string) error {
	spec, ok := LookupReaction(id)
	if !ok {
		return fmt.Errorf("config: %q is not a configurable reaction", id)
	}
	name = normaliseEmoji(name)
	if err := ValidateEmojiName(name); err != nil {
		return err
	}
	return c.setScalarSetting(spec.Path, name, true, func(cfg *Config) { spec.set(cfg, name) })
}

// ValidateEmojiName reports whether name is a plausible Slack shortcode.
//
// Empty is valid: it is how a reset is spelled.
//
// This cannot prove the workspace HAS the emoji — only Slack knows that, and it
// says so with `invalid_name` at the moment of use. What it can do is catch the
// two mistakes somebody actually makes at the modal, both of which would
// otherwise fail silently hours later on somebody else's approval:
//
//   - pasting the emoji CHARACTER rather than its name. `reactions.add` takes
//     `tada`, never 🎉, and the two are indistinguishable in a text box.
//   - pasting several, or a sentence. A name has no spaces in it.
func ValidateEmojiName(name string) error {
	name = normaliseEmoji(name)
	if name == "" {
		return nil
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '+', r == ':':
			// ':' survives normalisation only inside a skin-tone suffix
			// (`+1::skin-tone-3`), which is a legal name.
			continue
		case r >= 'A' && r <= 'Z':
			return fmt.Errorf("%q is not an emoji name: Slack shortcodes are lower case", name)
		case unicode.IsSpace(r):
			return fmt.Errorf("%q is not an emoji name: it has a space in it, so it is more than one word", name)
		default:
			return fmt.Errorf("%q is not an emoji name: type the shortcode (e.g. %s), not the emoji itself",
				name, DefaultSuccessEmoji)
		}
	}
	return nil
}

// normaliseEmoji trims whitespace and the colons a human writes around a
// shortcode, so `:tada:` and `tada` are the same setting.
// It is a copy of slack.NormaliseEmojiName rather than a call to it: internal
// /slack already imports this package to resolve a profile, so the dependency
// only runs one way. Four lines of duplication is the cheaper half of that
// trade — and the two are kept honest by the round trip in the tests, which
// puts a colon-wrapped name through this and asserts what reaches the API.
//
// Space, colons, space again. The last pass is not belt and braces: `: :`
// trims to a single space under the obvious two-step order, which is not empty
// and would then be validated as a name.
func normaliseEmoji(s string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(s), ":"))
}
