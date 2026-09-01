// Package schedule owns Riggs' own scheduler: what a job is, when it is due,
// and what running one means.
//
// It exists because Murtaugh used to own the schedule. Riggs was "always the
// callee" — Murtaugh held the cron, invoked `riggs git pr --bulk` every three
// minutes, and Riggs did one thing and exited. Phase 6 reversed half of that
// when Riggs got its own Slack app and its own inbound half; this reverses the
// rest, and Murtaugh stops being a dependency.
//
// The schedule lives INSIDE the daemon, which is the decision everything else
// here follows from. The alternative — one launchd agent or systemd timer per
// job — means two implementations, two calendar dialects that neither map onto
// cron nor onto each other, systemd user units that stop when you log out
// unless lingering is enabled, and nothing at all on a Linux without systemd.
// A ticker in a process that is already running is the same code on both, and
// the daemon has to be up regardless: if it is down, every button in every
// digest is dead and the jobs not firing is the least of the problem.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule reports when a job next runs.
type Schedule interface {
	// Next is the first due time strictly after t. A zero time means never,
	// which a calendar expression matching nothing produces.
	Next(t time.Time) time.Time
	// String renders the schedule as it was written.
	String() string
}

// Parse reads a schedule from a single field.
//
// Two dialects, told apart by shape rather than by a second setting:
//
//	3m              every three minutes, from whenever the daemon started
//	0 9 * * 1-5     at 09:00, Monday to Friday
//
// One field because a job has one schedule, and a form with "kind" and "value"
// invites the pair where kind says interval and value holds a cron expression.
// Murtaugh spelled these as two flags (`--every` and `--schedule`) and its jobs
// only ever used the first.
//
// Calendar expressions are evaluated in the machine's LOCAL time, deliberately.
// "09:00 on weekdays" means the operator's morning; a job that drifts an hour
// twice a year because it was pinned to UTC is a job nobody trusts.
func Parse(spec string) (Schedule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("a schedule is required (e.g. `3m`, or `0 9 * * 1-5`)")
	}
	// A duration first: it is the common case, and `3m` is not a valid cron
	// expression under any reading, so there is nothing to disambiguate.
	if d, err := time.ParseDuration(spec); err == nil {
		if d < MinInterval {
			return nil, fmt.Errorf("%s is too frequent; the minimum is %s", spec, MinInterval)
		}
		return Interval{Every: d, text: spec}, nil
	}
	if fields := strings.Fields(spec); len(fields) == 5 {
		return parseCron(spec, fields)
	}
	return nil, fmt.Errorf("%q is neither a duration (e.g. `3m`, `2h`) nor a five-field calendar expression (e.g. `0 9 * * 1-5`)", spec)
}

// MinInterval is the shortest interval a job may run at.
//
// One minute. Not because anything here cannot go faster, but because every
// job is a process spawn and the things Riggs schedules are governed by a
// three-hour cooldown (§9b) — the tick rate only bounds how quickly a new pull
// request first appears. A job configured for `10s` is a mistake being made in
// a modal, and the modal should say so rather than honour it.
const MinInterval = time.Minute

// Interval is a fixed-period schedule.
type Interval struct {
	Every time.Duration
	text  string
}

// Next is one period after t.
func (i Interval) Next(t time.Time) time.Time { return t.Add(i.Every) }

// String renders the interval as written.
func (i Interval) String() string {
	if i.text != "" {
		return i.text
	}
	return i.Every.String()
}

// Cron is a five-field calendar expression: minute, hour, day of month, month,
// day of week.
//
// Hand-written rather than taken from a library. The supported grammar is
// `*`, `n`, `a-b`, `*/s`, `a-b/s` and comma-separated lists of those — which is
// the whole of what anybody writes, and small enough to be read and tested in
// one sitting. Names (`MON`, `JAN`) are deliberately absent: they are the half
// of cron syntax that varies between implementations, and a job that runs on
// the wrong day because two parsers disagreed about `SUN` is precisely the bug
// this package must not have.
type Cron struct {
	minute, hour, dom, month, dow field
	text                          string
}

// field is one column of a cron expression: the set of values it matches.
type field struct {
	// set is indexed by value, offset by min. A nil set matches everything,
	// which is `*` — kept distinct from "matches nothing" so an empty list is
	// an error rather than a schedule that never fires.
	set []bool
	min int
	// star records that this field was written as `*`. Day-of-month and
	// day-of-week both need it: cron's oldest wart is that when BOTH are
	// restricted the match is a UNION, not an intersection.
	star bool
}

// matches reports whether v is in the field.
func (f field) matches(v int) bool {
	if f.set == nil {
		return true
	}
	i := v - f.min
	return i >= 0 && i < len(f.set) && f.set[i]
}

// cronBounds are the inclusive ranges of each column.
var cronBounds = [5][2]int{
	{0, 59}, // minute
	{0, 23}, // hour
	{1, 31}, // day of month
	{1, 12}, // month
	{0, 7},  // day of week, with 7 meaning Sunday as well as 0
}

// parseCron builds a Cron from its five columns.
func parseCron(text string, fields []string) (Cron, error) {
	parsed := make([]field, 5)
	for i, raw := range fields {
		f, err := parseField(raw, cronBounds[i][0], cronBounds[i][1])
		if err != nil {
			return Cron{}, fmt.Errorf("%s in %q: %w", columnNames[i], text, err)
		}
		parsed[i] = f
	}
	return Cron{
		minute: parsed[0], hour: parsed[1], dom: parsed[2],
		month: parsed[3], dow: parsed[4], text: text,
	}, nil
}

// columnNames name the five columns, for an error that says which one is wrong.
var columnNames = [5]string{"the minute field", "the hour field",
	"the day-of-month field", "the month field", "the day-of-week field"}

// parseField reads one column.
func parseField(raw string, min, max int) (field, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return field{}, fmt.Errorf("is empty")
	}
	if raw == "*" {
		return field{min: min, star: true}, nil
	}
	f := field{set: make([]bool, max-min+1), min: min}
	for _, part := range strings.Split(raw, ",") {
		if err := f.add(part, min, max); err != nil {
			return field{}, err
		}
	}
	for _, on := range f.set {
		if on {
			return f, nil
		}
	}
	// Reachable through a step larger than its own range, e.g. `*/70` on
	// minutes. A schedule that can never fire is a typo, not an instruction.
	return field{}, fmt.Errorf("%q matches nothing", raw)
}

// add folds one comma-separated term into the field.
func (f *field) add(part string, min, max int) error {
	part = strings.TrimSpace(part)
	step := 1
	if slash := strings.Index(part, "/"); slash >= 0 {
		var err error
		if step, err = strconv.Atoi(strings.TrimSpace(part[slash+1:])); err != nil || step <= 0 {
			return fmt.Errorf("%q has an invalid step", part)
		}
		// Vixie cron accepts a step wider than the field and quietly collapses
		// it to the first value: `*/70` on minutes means "minute 0". That is
		// indistinguishable from a typo, and the operator who meant minute 0
		// could have written `0`. Refused here rather than honoured.
		if step > max-min {
			return fmt.Errorf("%q steps by more than the whole %d-%d range", part, min, max)
		}
		part = strings.TrimSpace(part[:slash])
	}

	lo, hi := min, max
	switch {
	case part == "*":
		// The whole range, stepped.
	case strings.Contains(part, "-"):
		bounds := strings.SplitN(part, "-", 2)
		var err error
		if lo, err = boundOf(bounds[0], min, max); err != nil {
			return err
		}
		if hi, err = boundOf(bounds[1], min, max); err != nil {
			return err
		}
		if lo > hi {
			return fmt.Errorf("%q counts backwards", part)
		}
	default:
		v, err := boundOf(part, min, max)
		if err != nil {
			return err
		}
		lo, hi = v, v
	}

	for v := lo; v <= hi; v += step {
		f.set[v-min] = true
	}
	return nil
}

// boundOf parses one number and range-checks it.
func boundOf(s string, min, max int) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	if v < min || v > max {
		return 0, fmt.Errorf("%d is outside %d-%d", v, min, max)
	}
	return v, nil
}

// String renders the expression as written.
func (c Cron) String() string { return c.text }

// cronHorizon bounds the search for the next due time.
//
// Five years. An expression like `0 0 30 2 *` — the thirtieth of February —
// parses perfectly and matches no date that will ever exist, and something has
// to stop looking. The search skips a whole day at a time when the date does
// not match, so this costs a few thousand comparisons rather than the two and a
// half million minutes it spans.
const cronHorizon = 5 * 365 * 24 * time.Hour

// Next is the first minute strictly after t that the expression matches.
func (c Cron) Next(t time.Time) time.Time {
	// Truncated to the minute and advanced by one: a cron expression has no
	// sub-minute resolution, and "strictly after" is what stops a job that took
	// under a second from matching its own start minute again.
	next := t.Truncate(time.Minute).Add(time.Minute)
	limit := t.Add(cronHorizon)

	for next.Before(limit) {
		if !c.matchesDate(next) {
			// A whole day at a time. Midnight of the next day, computed through
			// the calendar rather than by adding 24h, so a daylight-saving
			// change does not shift the search off the hour.
			y, m, d := next.Date()
			next = time.Date(y, m, d+1, 0, 0, 0, 0, next.Location())
			continue
		}
		if c.hour.matches(next.Hour()) && c.minute.matches(next.Minute()) {
			return next
		}
		next = next.Add(time.Minute)
	}
	return time.Time{}
}

// matchesDate reports whether the day-of-month, month and day-of-week columns
// admit this date.
//
// The day fields are a UNION when both are restricted, which is cron's oldest
// and least defensible wart: `0 9 1 * 1` means "the first of the month, AND
// also every Monday", not "Mondays that fall on the first". It is reproduced
// here because reproducing it is the whole point of accepting cron syntax —
// somebody pasting an expression from a crontab must get what the crontab did.
func (c Cron) matchesDate(t time.Time) bool {
	if !c.month.matches(int(t.Month())) {
		return false
	}
	dom := c.dom.matches(t.Day())
	dow := c.dow.matches(int(t.Weekday())) || (int(t.Weekday()) == 0 && c.dow.matches(7))
	switch {
	case c.dom.star && c.dow.star:
		return true
	case c.dom.star:
		return dow
	case c.dow.star:
		return dom
	default:
		return dom || dow
	}
}
