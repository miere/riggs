package ticket

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// LegacyEntry is one record from the Python automation's state file
// (`automations/quick_coding_tasks/state/tickets.json`).
type LegacyEntry struct {
	Status      string  `json:"status"`
	Summary     string  `json:"summary"`
	Reporter    string  `json:"reporter"`
	Parent      string  `json:"parent"`
	TS          string  `json:"ts"`
	GoalSummary string  `json:"goal_summary"`
	LastNudgeTS float64 `json:"last_nudge_ts"`
}

// State maps the Python's status vocabulary onto ours. `resolved_oob` — the
// Python's "handled outside Slack" — is our Resolved.
func (e LegacyEntry) State() State {
	switch e.Status {
	case "pending":
		return Pending
	case "assigned":
		return Assigned
	case "dismissed":
		return Dismissed
	default:
		return Resolved
	}
}

// NudgedAt returns when the idle ping last fired, if ever. Carrying this over
// is what stops every stale card being nudged at once on the first run.
func (e LegacyEntry) NudgedAt() (time.Time, bool) {
	if e.LastNudgeTS <= 0 {
		return time.Time{}, false
	}
	sec := int64(e.LastNudgeTS)
	return time.Unix(sec, int64((e.LastNudgeTS-float64(sec))*1e9)), true
}

// LegacyState is the whole file, keyed by ticket key.
type LegacyState map[string]LegacyEntry

// LoadLegacyState reads the Python automation's state file.
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

// Keys returns the ticket keys in a stable order.
func (s LegacyState) Keys() []string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
