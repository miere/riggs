// CLI rendering for tool results. A tool may return a fmt.Stringer to drive
// human-readable output independently from the structured JSON shape the MCP
// frontend emits. Strings and []string are handled directly so simple tools
// don't need to allocate a Stringer.
package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Render converts a tool result into the text the CLI writes to stdout. The
// contract per tool is documented at the tool package; this is the dispatch
// site that picks the appropriate representation.
func Render(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []string:
		return strings.Join(x, "\n")
	case fmt.Stringer:
		return x.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

// RenderJSON encodes a tool result as JSON for machine consumption — the
// --json-output flag. Unlike Render, the output is always valid JSON: even a
// bare string result comes back quoted, so a downstream parser (Murtaugh's
// workflow rules shelling this binary) can rely on the shape. This is the same
// structured representation the MCP frontend emits; the tool result types
// carry the json tags that define it.
func RenderJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
