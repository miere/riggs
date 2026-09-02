// CLI rendering for command results. A result may be a fmt.Stringer, which
// drives human-readable output independently of the structured JSON shape.
// Strings and []string are handled directly so a simple result need not
// allocate a Stringer.
package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Render converts a result into the text the CLI writes to stdout.
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

// RenderJSON encodes a result as JSON for machine consumption — the
// --json-output flag. Unlike Render, the output is always valid JSON: even a
// bare string comes back quoted, so anything parsing this binary's output can
// rely on the shape. The result types carry the json tags that define it.
func RenderJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// render writes one result to stdout, in whichever representation was asked for.
func (f *Frontend) render(result any, asJSON bool) error {
	if !asJSON {
		if text := Render(result); text != "" {
			fmt.Fprintln(f.stdout, text)
		}
		return nil
	}
	encoded, err := RenderJSON(result)
	if err != nil {
		return err
	}
	fmt.Fprintln(f.stdout, encoded)
	return nil
}
