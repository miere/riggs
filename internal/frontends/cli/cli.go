// Package cli is the human-facing frontend: it maps the two digest commands
// onto the calls that run them and writes the results to stdout. Diagnostics
// go to stderr; stdout is reserved for output.
//
// It used to be generic. A tool registry held every command, each tool declared
// a jsonschema, and this package parsed flags by looking each one up in that
// schema — which is how one frontend served both the CLI and an MCP server
// without either knowing what the other exposed.
//
// Both halves of that justification are gone. Nothing registers Riggs as an MCP
// server, and the registry is down to the two digests. Two commands with the
// same shape do not need a symbol table to be parsed, so the schema, the
// registry and the three-form command resolution went and this is what is left.
//
// The command spellings are a CONTRACT, not a convenience. Jobs stored in the
// ledger invoke `git pr --bulk <login>` as literal argv and the scheduler
// validates nothing (internal/schedule/exec.go), so a rename here does not fail
// to build — it fails at 3am, in a job, with "unknown command".
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// ErrUsage is returned for no arguments or an unknown command. Callers map it
// to a non-zero exit and a usage message on stderr.
var ErrUsage = errors.New("usage: riggs <command>")

// jsonOutputFlag switches rendering from human-readable text to JSON. It is a
// frontend flag rather than a parameter, so it may appear anywhere.
const jsonOutputFlag = "--json-output"

// Invoker runs one command with the arguments parsed for it.
//
// The two digests still take a `map[string]any`, which is what the registry
// handed them. Keeping that shape means this rewrite touches the frontend and
// nothing else — the tools, their defaults and their tests are untouched.
type Invoker interface {
	Invoke(ctx context.Context, args map[string]any) (any, error)
}

// Frontend is the CLI adapter.
type Frontend struct {
	// reviews and tickets are the two commands. Either may be nil, which is
	// what an unconfigured machine looks like: the digests need a Slack
	// account to post through, and `riggs capabilities` explains the absence.
	reviews Invoker
	tickets Invoker
	stdout  io.Writer
	stderr  io.Writer
}

// New constructs a Frontend over the two digests.
func New(reviews, tickets Invoker) *Frontend {
	return &Frontend{reviews: reviews, tickets: tickets, stdout: os.Stdout, stderr: os.Stderr}
}

// WithOutput overrides the output streams; intended for tests.
func (f *Frontend) WithOutput(stdout, stderr io.Writer) *Frontend {
	f.stdout, f.stderr = stdout, stderr
	return f
}

// Run dispatches one command line.
func (f *Frontend) Run(ctx context.Context, args []string) error {
	asJSON, args := extractJSONFlag(args)
	if len(args) == 0 {
		return ErrUsage
	}

	tool, rest, err := f.resolve(args)
	if err != nil {
		return err
	}
	parsed, err := parseArgs(rest)
	if err != nil {
		return err
	}
	result, err := tool.Invoke(ctx, parsed)
	if err != nil {
		return err
	}
	return f.render(result, asJSON)
}

// resolve maps the leading tokens onto a command.
//
// Exact matches only, and only these two. The old frontend accepted a flat
// name, a dotted namespace and a verb flag, resolved longest-prefix-first
// against whatever happened to be registered; with two commands that machinery
// was answering a question nobody was asking.
func (f *Frontend) resolve(args []string) (Invoker, []string, error) {
	switch {
	case len(args) >= 3 && args[0] == "git" && args[1] == "pr" && args[2] == "--bulk":
		if f.reviews == nil {
			return nil, nil, fmt.Errorf("the pull-request digest is not configured; run `riggs capabilities`")
		}
		return f.reviews, append([]string{"user"}, args[3:]...), nil

	case len(args) >= 3 && args[0] == "jira" && args[1] == "tickets" && args[2] == "--bulk":
		if f.tickets == nil {
			return nil, nil, fmt.Errorf("the ticket digest is not configured; run `riggs capabilities`")
		}
		return f.tickets, append([]string{"query"}, args[3:]...), nil
	}
	return nil, nil, fmt.Errorf("%w: unknown command: %s", ErrUsage, strings.Join(args, " "))
}

// parseArgs reads a command's primary value and its flags.
//
// rest[0] names the primary parameter, rest[1] is its value when there is one,
// and everything after that is `--kebab-flag value`. A flag with no declared
// type is passed through as a string; the two that are not strings are named
// here rather than derived, because there are two of them.
func parseArgs(rest []string) (map[string]any, error) {
	out := map[string]any{}
	name := rest[0]
	rest = rest[1:]

	if len(rest) > 0 && !strings.HasPrefix(rest[0], "--") {
		out[name] = rest[0]
		rest = rest[1:]
	}

	for i := 0; i < len(rest); i++ {
		token := rest[i]
		if !strings.HasPrefix(token, "--") {
			return nil, fmt.Errorf("unexpected argument %q", token)
		}
		key, value, inline := strings.Cut(strings.TrimPrefix(token, "--"), "=")
		key = strings.ReplaceAll(key, "-", "_")

		if key == "dry_run" {
			// The one boolean, and valueless: `--dry-run`, never `--dry-run true`.
			out[key] = true
			continue
		}
		if !inline {
			if i+1 >= len(rest) {
				return nil, fmt.Errorf("--%s requires a value", strings.ReplaceAll(key, "_", "-"))
			}
			i++
			value = rest[i]
		}
		if key == "max_items" {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("--max-items %q is not a number", value)
			}
			out[key] = n
			continue
		}
		out[key] = value
	}
	return out, nil
}

// extractJSONFlag removes every occurrence of --json-output, reporting whether
// it was present. The returned slice never aliases the caller's.
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
