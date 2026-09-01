package schedule

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Running a job means running THIS binary again, as a child process.
//
// Not an in-process call through the tool registry, which would be cheaper and
// is the obvious first idea. Three reasons it is not the right one:
//
//   - The argv is byte-identical to what Murtaugh ran. The migration off it
//     changes when a job fires and nothing whatsoever about what it does, which
//     is the only way to tell a scheduling bug from a behaviour change.
//   - A job that hangs, leaks or crashes cannot take the daemon with it, and a
//     timeout is a signal to a process rather than a goroutine politely
//     noticing a cancelled context — which a blocked syscall never does.
//   - The tools already assume they own the process: they open the ledger,
//     do one thing and exit. Running twenty of them inside a long-lived daemon
//     is a different set of assumptions than the ones they were written under.
//
// The cost is a process spawn every three minutes, which is nothing.

// SelfExec builds an Exec that runs this binary again.
//
// configPath is passed on as `--config-file` only when it is not where Riggs
// would look anyway, which is the same rule the Murtaugh installer followed:
// the common case stays readable in a log line.
//
// The child inherits this process's environment verbatim. That is load-bearing
// under launchd: the daemon's own environment is the one the launch agent was
// given — the captured PATH, and whatever `env-file` resolved (§12b) — and a
// job started with anything less would fail on `gh` not being found, having
// worked perfectly when run by hand.
func SelfExec(configPath string) (Exec, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("schedule: cannot find this binary to run jobs with: %w", err)
	}
	return func(ctx context.Context, args []string) ([]byte, error) {
		return runProcess(ctx, self, argsFor(args, configPath))
	}, nil
}

// argsFor appends the config flag when the config is not in its default place.
func argsFor(args []string, configPath string) []string {
	out := append([]string{}, args...)
	if configPath != "" {
		out = append(out, "--config-file", configPath)
	}
	return out
}

// runProcess executes the child and returns its combined output.
//
// Combined, because a job that failed says why on whichever stream it feels
// like and a reader looking at "it failed" wants whichever one that was.
func runProcess(ctx context.Context, bin string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

// tailOf keeps the last lines and characters of s.
func tailOf(s string, maxLines, maxChars int) string {
	out := strings.TrimSpace(s)
	if out == "" {
		return ""
	}
	lines := strings.Split(out, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	out = strings.Join(lines, "\n")
	if runes := []rune(out); len(runes) > maxChars {
		out = "…" + string(runes[len(runes)-maxChars:])
	}
	// Backticks would close the fence this is wrapped in on its way to Slack
	// and spill the rest of the output into the message as markup.
	return strings.ReplaceAll(out, "```", "'''")
}
