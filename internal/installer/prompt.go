package installer

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Prompter is the console seam. Everything the installer asks goes through it,
// so the whole flow is drivable by a scripted fake and the tests never need a
// terminal.
type Prompter interface {
	// Ask reads a line, returning def when the answer is empty.
	Ask(question, def string) (string, error)
	// AskSecret reads a line without echoing it.
	AskSecret(question string) (string, error)
	// Confirm reads a yes/no answer.
	Confirm(question string, def bool) (bool, error)
	// Say writes a line of narration.
	Say(format string, args ...any)
}

// Console is the interactive Prompter.
type Console struct {
	in  *bufio.Reader
	out io.Writer
	// fd is the file descriptor used to disable echo. -1 means "not a
	// terminal", in which case secrets are read as ordinary lines.
	fd int
}

// NewConsole builds a Prompter over stdin/stdout.
func NewConsole() *Console {
	fd := -1
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fd = int(os.Stdin.Fd())
	}
	return &Console{in: bufio.NewReader(os.Stdin), out: os.Stdout, fd: fd}
}

// IsTerminal reports whether stdin is a terminal. The installer refuses to run
// without one, rather than silently echoing a pasted token into the scrollback.
func (c *Console) IsTerminal() bool { return c.fd >= 0 }

// Say writes narration to stdout.
func (c *Console) Say(format string, args ...any) {
	fmt.Fprintf(c.out, format+"\n", args...)
}

// Ask reads a line, falling back to def when the answer is empty.
func (c *Console) Ask(question, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(c.out, "%s [%s]: ", question, def)
	} else {
		fmt.Fprintf(c.out, "%s: ", question)
	}
	line, err := c.in.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

// AskSecret reads a line with the terminal echo disabled.
func (c *Console) AskSecret(question string) (string, error) {
	fmt.Fprintf(c.out, "%s: ", question)
	if c.fd < 0 {
		// No terminal to silence. Read the line, but say so — pretending it
		// was hidden would be worse than admitting it was not.
		line, err := c.in.ReadString('\n')
		fmt.Fprintln(c.out, "  (warning: input was not hidden — stdin is not a terminal)")
		return strings.TrimSpace(line), err
	}
	raw, err := term.ReadPassword(c.fd)
	fmt.Fprintln(c.out)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// Confirm reads a yes/no answer.
func (c *Console) Confirm(question string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		fmt.Fprintf(c.out, "%s [%s]: ", question, hint)
		line, err := c.in.ReadString('\n')
		if err != nil && err != io.EOF {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		if err == io.EOF {
			return def, nil
		}
		fmt.Fprintln(c.out, "  please answer y or n")
	}
}

// redact renders a secret for display: enough to recognise, not enough to use.
func redact(secret string) string {
	switch {
	case secret == "":
		return "(empty)"
	case strings.HasPrefix(secret, "${"):
		return secret // an env reference is not a secret
	case len(secret) <= 8:
		return strings.Repeat("*", len(secret))
	default:
		return secret[:4] + strings.Repeat("*", len(secret)-8) + secret[len(secret)-4:]
	}
}
