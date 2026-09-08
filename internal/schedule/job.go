package schedule

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/miere/riggs-mcp/internal/notify"
)

// Job is the record the scheduler works from. It is the ledger's row (§9c),
// used directly rather than copied into a parallel struct that would have to be
// kept in step with it.
type Job = notify.Job

// DefaultTimeout bounds a run whose job did not say.
//
// Two minutes, which is what Murtaugh gave both of its jobs. It is enough for a
// digest pass — one GitHub search, a handful of conditional reads, one Slack
// call — and short enough that a wedged one is skipped rather than blocking the
// next tick for the rest of the afternoon.
const DefaultTimeout = 2 * time.Minute

// MaxTimeout is the longest a job may be given.
//
// An hour. Not a technical limit: a job that needs longer than an hour is not a
// scheduled task, it is a service, and it should be supervised as one rather
// than restarted from a ticker every time it fails to finish.
const MaxTimeout = time.Hour

// namePattern is what a job may be called.
//
// Constrained because the name is the job's identity in three places at once:
// the ledger's primary key, the `block_id` of its row on the Home tab, and the
// word in every log line about it. Spaces and punctuation survive none of those
// equally well.
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// ValidateName reports whether name may identify a job.
func ValidateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("a job needs a name")
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%q is not a usable job name: letters, digits, dot, dash and underscore, up to 64 characters", name)
	}
	return nil
}

// NewJob validates and assembles a job from what a modal or a command line
// supplied.
//
// args is the argument list for the riggs binary, already split. spec is the
// schedule in either dialect. A zero timeout takes DefaultTimeout.
//
// Everything is checked here, in one place, because there are two front doors —
// the Home tab's modal and `riggs jobs add` — and a rule enforced in only one
// of them is a rule that is not enforced.
func NewJob(name string, args []string, spec string, timeout time.Duration, enabled bool) (Job, error) {
	if err := ValidateName(name); err != nil {
		return Job{}, err
	}
	if len(args) == 0 {
		return Job{}, fmt.Errorf("job %s has nothing to run (e.g. `git pr --bulk miere`)", name)
	}
	if _, err := Parse(spec); err != nil {
		return Job{}, fmt.Errorf("job %s: %w", name, err)
	}
	switch {
	case timeout == 0:
		timeout = DefaultTimeout
	case timeout < 0:
		return Job{}, fmt.Errorf("job %s: a timeout cannot be negative", name)
	case timeout > MaxTimeout:
		return Job{}, fmt.Errorf("job %s: %s is longer than the %s maximum; something that runs that long is a service, not a job",
			name, timeout, MaxTimeout)
	}
	return Job{
		Name: name, Args: args, Spec: strings.TrimSpace(spec),
		Timeout: timeout, Enabled: enabled,
	}, nil
}

// SplitArgs reads a command line into an argument list, honouring quotes.
//
// It used to split on whitespace and nothing else, on the reasoning that
// anything needing more wanted a wrapper script. That reasoning was wrong about
// the one command Riggs actually schedules. A ticket digest IS its JQL —
//
//	jira tickets --bulk 'project = NYX AND labels = "ai-able" AND status = "Ready"'
//
// — and JQL has its own quoting, which it needs, for values with spaces in
// them. Under the old rule that line arrived as twenty-two arguments and the
// child process died on `unexpected argument "="`. There is no wrapper script
// that fixes that, because the thing being mangled is the argument, not the
// command around it.
//
// The dialect is the one everybody already knows, and no more of it:
//
//   - single quotes are literal, right through to the closing quote
//   - double quotes take a backslash escape for `"` and `\`; any other
//     backslash inside them stays a backslash, so a Windows path or a JQL
//     regex does not quietly lose one
//   - outside quotes, a backslash escapes the next character
//   - unquoted whitespace separates arguments, and nothing else does
//
// No expansion of any kind: no `$VAR`, no globs, no backticks, no `#` comment.
// A job is argv handed to exec (§exec.go), never a line handed to a shell, and
// a quoting dialect that LOOKS like sh while silently declining to expand is
// worse than one that plainly does not.
//
// An unterminated quote is an error rather than a best guess. The guess is
// always "the operator meant the rest of the line", which is right about half
// the time and silently ships a wrong query the other half.
//
// A leading `riggs` is dropped. The binary is not the operator's to choose —
// every job runs THIS build, at the path this daemon was started from — and
// typing the whole command Murtaugh used to run is the obvious thing to do.
func SplitArgs(command string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		started bool // distinguishes an empty argument ('') from no argument
	)
	push := func() {
		if started {
			args = append(args, cur.String())
			cur.Reset()
			started = false
		}
	}

	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case unicode.IsSpace(c):
			push()

		case c == '\'':
			started = true
			end := indexRune(runes, i+1, '\'')
			if end < 0 {
				return nil, unterminated('\'', command)
			}
			cur.WriteString(string(runes[i+1 : end]))
			i = end

		case c == '"':
			started = true
			j := i + 1
			for ; j < len(runes) && runes[j] != '"'; j++ {
				// Only `"` and `\` are escapable in here. Anything else keeps
				// its backslash, so `\d` survives into a regex intact.
				if runes[j] == '\\' && j+1 < len(runes) &&
					(runes[j+1] == '"' || runes[j+1] == '\\') {
					j++
				}
				cur.WriteRune(runes[j])
			}
			if j >= len(runes) {
				return nil, unterminated('"', command)
			}
			i = j

		case c == '\\':
			if i+1 >= len(runes) {
				return nil, fmt.Errorf("this command ends in a backslash with nothing to escape: %s", command)
			}
			started = true
			i++
			cur.WriteRune(runes[i])

		default:
			started = true
			cur.WriteRune(c)
		}
	}
	push()
	return TrimBinary(args), nil
}

// indexRune finds want in runes at or after from, or -1.
func indexRune(runes []rune, from int, want rune) int {
	for i := from; i < len(runes); i++ {
		if runes[i] == want {
			return i
		}
	}
	return -1
}

// unterminated names the quote that was never closed. The message quotes the
// character itself, because "unterminated quote" on a line containing both
// kinds leaves the reader to guess which one this parser cared about.
func unterminated(quote rune, command string) error {
	return fmt.Errorf("this command has an unterminated %c quote: %s", quote, command)
}

// TrimBinary drops a leading `riggs` from an argument list.
//
// Split out from SplitArgs because `riggs jobs add` never goes through a
// splitter at all: the shell has already produced the argv, correctly, and
// re-joining it to split it again is precisely how the JQL used to be lost.
// The one thing that rule still has to do is forgive the operator for typing
// the binary's name, so that is all that is left here.
func TrimBinary(args []string) []string {
	if len(args) > 0 && strings.EqualFold(args[0], "riggs") {
		return args[1:]
	}
	return args
}

// Command renders a job's arguments as the line somebody would type.
//
// Quoted where quoting is needed, and that is not cosmetic: this string is what
// the Home tab's edit modal is PREFILLED with (internal/apphome/jobs.go), and
// whatever comes back from that form goes through SplitArgs. Rendered bare, a
// job whose JQL was right would come apart the first time somebody opened it to
// change the schedule and pressed Save — a silent edit, to a field nobody
// touched. Round-tripping through SplitArgs is the property being defended.
func Command(job Job) string {
	quoted := make([]string, len(job.Args))
	for i, a := range job.Args {
		quoted[i] = QuoteArg(a)
	}
	return strings.Join(quoted, " ")
}

// QuoteArg renders one argument so SplitArgs reads it back unchanged.
//
// Only the four characters SplitArgs treats specially force quoting —
// whitespace, both quotes, and the backslash — so ordinary tokens stay bare and
// the line still reads like something a person typed. Single quotes are
// preferred because JQL's own quoting is double, and `'...'` leaves it visible
// rather than burying it under backslashes.
func QuoteArg(arg string) string {
	if arg == "" {
		return "''"
	}
	if !strings.ContainsFunc(arg, needsQuoting) {
		return arg
	}
	if !strings.Contains(arg, "'") {
		return "'" + arg + "'"
	}
	// A single quote cannot be escaped inside single quotes, in this dialect or
	// in sh. Close, emit an escaped one, reopen — the same `'\''` sh uses.
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// needsQuoting reports whether c would change how SplitArgs reads a token.
func needsQuoting(c rune) bool {
	return unicode.IsSpace(c) || c == '\'' || c == '"' || c == '\\'
}
