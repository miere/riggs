package pullrequest

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// LegacyEntry is one record from the Python mirror's state file
// (`automations/pull_request/state/github_review_queue.json`).
//
// Reviewable is a *pointer* on purpose: the Python's migration path wrote null
// for entries whose state it could not translate, and null means "unknown",
// not "not reviewable". Collapsing the two would silently change what the
// parity check is comparing against.
type LegacyEntry struct {
	TS         string  `json:"ts"`
	Channel    string  `json:"channel"`
	Tagged     bool    `json:"tagged"`
	Summary    *string `json:"summary"`
	Detail     string  `json:"detail"`
	Reviewable *bool   `json:"reviewable"`
	Reason     *string `json:"reason"`
}

// Token renders the legacy entry's state in the same vocabulary Riggs uses, so
// the two can be compared directly. Unknown stays empty.
func (e LegacyEntry) Token() string {
	if e.Reviewable != nil && *e.Reviewable {
		return "reviewable"
	}
	if e.Reason != nil {
		return *e.Reason
	}
	return ""
}

// Terminal reports whether the legacy entry had reached a final state, and so
// will never be re-fetched.
func (e LegacyEntry) Terminal() bool { return TerminalReasons[e.Token()] }

// LegacyState is the whole file, keyed by "owner/repo#number".
type LegacyState map[string]LegacyEntry

// LoadLegacyState reads the Python mirror's state file.
func LoadLegacyState(path string) (LegacyState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var state LegacyState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return state, nil
}

// Refs returns the keys in a stable order.
func (s LegacyState) Refs() []string {
	refs := make([]string, 0, len(s))
	for ref := range s {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}

// Live returns the refs that are not yet terminal — the only ones whose state
// can still change, and therefore the only ones worth checking or re-fetching.
func (s LegacyState) Live() []string {
	var refs []string
	for _, ref := range s.Refs() {
		if !s[ref].Terminal() {
			refs = append(refs, ref)
		}
	}
	return refs
}
