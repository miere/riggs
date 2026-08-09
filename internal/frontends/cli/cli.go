// Package cli implements the human-facing frontend. It maps subcommands to
// tools registered in the shared tool registry and writes their results to
// stdout. Diagnostics go to stderr; stdout is reserved for tool output.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/miere/riggs-mcp/internal/tools"
)

// ErrUsage is returned when the user invokes the CLI without arguments or with
// an unknown command. Callers map it to a non-zero exit and a usage message on
// stderr.
var ErrUsage = errors.New("usage: riggs <command>")

// jsonOutputFlag is a reserved, valueless frontend flag that switches CLI
// rendering from human-readable text to machine-parseable JSON. It is not a
// tool parameter, so it is stripped from args before schema flag parsing, and
// may appear anywhere in the command line.
const jsonOutputFlag = "--json-output"

// extractJSONFlag removes every occurrence of --json-output from args,
// reporting whether it was present and returning the remaining tokens. The
// flag carries no value, so the token is simply dropped. The returned slice is
// freshly allocated and never aliases the caller's backing array.
func extractJSONFlag(args []string) (bool, []string) {
	rest := make([]string, 0, len(args))
	found := false
	for _, a := range args {
		if a == jsonOutputFlag {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return found, rest
}

// Frontend is the CLI adapter.
type Frontend struct {
	registry *tools.Registry
	stdout   io.Writer
	stderr   io.Writer
}

// New constructs a CLI Frontend that writes to os.Stdout and os.Stderr.
func New(reg *tools.Registry) *Frontend {
	return &Frontend{registry: reg, stdout: os.Stdout, stderr: os.Stderr}
}

// WithOutput overrides the output streams; intended for tests.
func (f *Frontend) WithOutput(stdout, stderr io.Writer) *Frontend {
	f.stdout, f.stderr = stdout, stderr
	return f
}

// Run executes the command described by args. See resolve for how a tool name
// is recovered from the command line.
//
// Remaining tokens after the resolved name are parsed as --flag VALUE pairs
// against the tool's InputSchema and passed as the args map. The tool's result
// is rendered via Render (or RenderJSON when --json-output is present) and
// written to stdout followed by a newline.
func (f *Frontend) Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return ErrUsage
	}
	jsonOut, args := extractJSONFlag(args)
	if len(args) == 0 {
		return ErrUsage
	}
	res, err := f.resolve(args)
	if err != nil {
		return err
	}
	tool, _ := f.registry.Get(res.name)
	parsed, err := parseFlags(tool.InputSchema(), res.rest)
	if err != nil {
		return fmt.Errorf("%s: %w", res.name, err)
	}
	if res.primaryProp != "" && res.primaryValue != "" {
		if parsed == nil {
			parsed = map[string]any{}
		}
		parsed[res.primaryProp] = res.primaryValue
	}
	result, err := tool.Invoke(ctx, parsed)
	if err != nil {
		return fmt.Errorf("%s: %w", res.name, err)
	}
	out := Render(result)
	if jsonOut {
		if out, err = RenderJSON(result); err != nil {
			return fmt.Errorf("%s: json-output: %w", res.name, err)
		}
	}
	if _, err := fmt.Fprintln(f.stdout, out); err != nil {
		return err
	}
	return nil
}

// resolution is the outcome of matching a command line against the registry.
type resolution struct {
	name string   // the registered tool name
	rest []string // tokens to parse as --flag VALUE pairs
	// primaryProp/primaryValue carry a verb flag's value into the args map,
	// e.g. `--approve owner/repo#1` binding "owner/repo#1" to the "pr"
	// property. Empty when the command did not resolve through a verb flag.
	primaryProp  string
	primaryValue string
}

// resolve picks the registered tool name out of args. Three forms are tried,
// most literal first:
//
//  1. flat        — `riggs ping`            → "ping"
//  2. dotted      — `riggs jira tickets`    → "jira.tickets"
//  3. verb flag   — `riggs git pr --approve <ref>` → "git.pr.approve"
//
// The third form exists because the verb reads better as a flag than as a
// third positional token, and it lets the operation carry its primary argument
// as the flag's own value — which keeps the "every flag has a value, no
// positional arguments" rule of the parser intact.
//
// A verb flag is matched against the *registry*, not a list: `<prefix>.<verb>`
// must be a registered tool, so an ordinary parameter flag can never be
// mistaken for a verb.
func (f *Frontend) resolve(args []string) (resolution, error) {
	if _, ok := f.registry.Get(args[0]); ok {
		return resolution{name: args[0], rest: args[1:]}, nil
	}
	if len(args) >= 2 {
		dotted := args[0] + "." + args[1]
		if _, ok := f.registry.Get(dotted); ok {
			return resolution{name: dotted, rest: args[2:]}, nil
		}
	}
	// Verb-flag form. Try the longest namespace prefix first so `git pr` wins
	// over `git` when both could match.
	for n := min(2, len(args)); n >= 1; n-- {
		prefix := strings.Join(args[:n], ".")
		if res, ok := f.resolveVerb(prefix, args[n:]); ok {
			return res, nil
		}
	}
	return resolution{}, fmt.Errorf("unknown command: %s", strings.Join(args, " "))
}

// resolveVerb scans rest for the first --flag naming a tool registered under
// prefix. On a hit it removes the verb (and its value, if it took one) from
// the token list and reports the primary-argument binding.
func (f *Frontend) resolveVerb(prefix string, rest []string) (resolution, bool) {
	for i, tok := range rest {
		if !strings.HasPrefix(tok, "--") {
			continue
		}
		verb := strings.TrimPrefix(tok, "--")
		if eq := strings.Index(verb, "="); eq >= 0 {
			verb = verb[:eq]
		}
		name := prefix + "." + verb
		tool, ok := f.registry.Get(name)
		if !ok {
			continue
		}

		prop := ""
		if vt, ok := tool.(tools.VerbTool); ok {
			prop = vt.PrimaryArg()
		}

		// The verb's value, when it takes one: either --verb=VALUE or the
		// following token, provided that token is not itself a flag (the
		// primary argument may legitimately be omitted).
		value := ""
		consumed := 1
		if eq := strings.Index(rest[i], "="); eq >= 0 {
			value = rest[i][eq+1:]
		} else if prop != "" && i+1 < len(rest) && !strings.HasPrefix(rest[i+1], "--") {
			value = rest[i+1]
			consumed = 2
		}

		remaining := make([]string, 0, len(rest)-consumed)
		remaining = append(remaining, rest[:i]...)
		remaining = append(remaining, rest[i+consumed:]...)
		return resolution{name: name, rest: remaining, primaryProp: prop, primaryValue: value}, true
	}
	return resolution{}, false
}
