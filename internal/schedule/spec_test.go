package schedule

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func mustParse(t *testing.T, spec string) Schedule {
	t.Helper()
	s, err := Parse(spec)
	if err != nil {
		t.Fatalf("Parse(%q): %v", spec, err)
	}
	return s
}

// One field, two dialects, told apart by shape. A form with "kind" and "value"
// invites the pair where kind says interval and value holds a cron expression.
func TestParseTellsTheDialectsApart(t *testing.T) {
	if _, ok := mustParse(t, "3m").(Interval); !ok {
		t.Fatal("3m did not parse as an interval")
	}
	if _, ok := mustParse(t, "0 9 * * 1-5").(Cron); !ok {
		t.Fatal("a five-field expression did not parse as cron")
	}
	// The schedule survives the round trip as written, because that is what the
	// Home tab shows and what the operator typed.
	if got := mustParse(t, "0 9 * * 1-5").String(); got != "0 9 * * 1-5" {
		t.Fatalf("String = %q", got)
	}
}

// An unparseable schedule names both forms. "invalid schedule" tells somebody
// staring at a modal nothing they can act on.
func TestParseRejectsNonsenseHelpfully(t *testing.T) {
	for _, spec := range []string{"", "   ", "every 3 minutes", "0 9 * *", "0 9 * * 1-5 6"} {
		_, err := Parse(spec)
		if err == nil {
			t.Fatalf("Parse(%q) succeeded", spec)
		}
	}
	if _, err := Parse("banana"); err == nil {
		t.Fatal("Parse(banana) succeeded")
	} else if got := err.Error(); !contains(got, "3m") || !contains(got, "0 9 * * 1-5") {
		t.Fatalf("the error names neither form: %v", err)
	}
}

// Every job is a process spawn, and the things Riggs schedules are governed by
// a three-hour cooldown. `10s` is a mistake being made in a modal.
func TestParseRefusesASubMinuteInterval(t *testing.T) {
	if _, err := Parse("10s"); err == nil {
		t.Fatal("Parse(10s) succeeded")
	}
	if _, err := Parse("1m"); err != nil {
		t.Fatalf("Parse(1m): %v", err)
	}
}

func TestIntervalNext(t *testing.T) {
	s := mustParse(t, "3m")
	if got := s.Next(at("2026-09-01 09:00")); !got.Equal(at("2026-09-01 09:03")) {
		t.Fatalf("Next = %v", got)
	}
}

func TestCronNext(t *testing.T) {
	for name, tc := range map[string]struct{ spec, from, want string }{
		"later the same day": {"0 9 * * *", "2026-09-01 07:30", "2026-09-01 09:00"},
		"tomorrow":           {"0 9 * * *", "2026-09-01 09:30", "2026-09-02 09:00"},
		// Strictly after: a job that took under a second must not match the
		// minute it started in and run again immediately.
		"never the same minute": {"0 9 * * *", "2026-09-01 09:00", "2026-09-02 09:00"},
		"weekdays only":         {"0 9 * * 1-5", "2026-09-04 09:30", "2026-09-07 09:00"},
		"every fifteen minutes": {"*/15 * * * *", "2026-09-01 09:07", "2026-09-01 09:15"},
		"a list of hours":       {"30 8,17 * * *", "2026-09-01 09:00", "2026-09-01 17:30"},
		"a day of the month":    {"0 0 1 * *", "2026-09-15 12:00", "2026-10-01 00:00"},
		"a month":               {"0 0 1 1 *", "2026-09-15 12:00", "2027-01-01 00:00"},
		// Sunday is 0, and 7 as well: both spellings are in the wild.
		"sunday as zero":  {"0 9 * * 0", "2026-09-01 12:00", "2026-09-06 09:00"},
		"sunday as seven": {"0 9 * * 7", "2026-09-01 12:00", "2026-09-06 09:00"},
	} {
		t.Run(name, func(t *testing.T) {
			got := mustParse(t, tc.spec).Next(at(tc.from))
			if !got.Equal(at(tc.want)) {
				t.Fatalf("Next(%s) = %v, want %s", tc.from, got, tc.want)
			}
		})
	}
}

// Cron's oldest wart: when day-of-month AND day-of-week are both restricted the
// match is a union. Reproducing it is the point of accepting cron syntax at all
// — somebody pasting an expression from a crontab must get what the crontab did.
func TestTheTwoDayFieldsAreAUnion(t *testing.T) {
	// The 1st of the month, and also every Monday.
	s := mustParse(t, "0 9 1 * 1")
	// 2026-09-01 is a Tuesday: matched by the day-of-month alone.
	if got := s.Next(at("2026-08-31 12:00")); !got.Equal(at("2026-09-01 09:00")) {
		t.Fatalf("the first of the month was missed: %v", got)
	}
	// 2026-09-07 is a Monday: matched by the day-of-week alone.
	if got := s.Next(at("2026-09-01 12:00")); !got.Equal(at("2026-09-07 09:00")) {
		t.Fatalf("the Monday was missed: %v", got)
	}
}

// An expression that parses and can never match has to stop being searched for.
func TestAnImpossibleDateGivesUp(t *testing.T) {
	if got := mustParse(t, "0 0 30 2 *").Next(at("2026-09-01 12:00")); !got.IsZero() {
		t.Fatalf("Next = %v, want the zero time", got)
	}
}

// A field that matches nothing is a typo, not an instruction.
func TestFieldsThatMatchNothingAreRefused(t *testing.T) {
	for _, spec := range []string{
		"*/70 * * * *", // a step wider than the range
		"60 * * * *",   // minutes stop at 59
		"* 24 * * *",   // hours stop at 23
		"* * 0 * *",    // there is no day zero
		"* * * 13 *",   // there is no thirteenth month
		"5-1 * * * *",  // counts backwards
		"*/0 * * * *",  // a zero step
		"a * * * *",    // not a number
	} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) succeeded", spec)
		}
	}
}

// The error says which column is wrong. A five-field expression has five ways
// to be wrong and they look alike.
func TestACronErrorNamesItsColumn(t *testing.T) {
	_, err := Parse("* 24 * * *")
	if err == nil {
		t.Fatal("Parse succeeded")
	}
	if !contains(err.Error(), "hour") {
		t.Fatalf("err = %v, want it to name the hour field", err)
	}
}

// Skipping a non-matching date jumps to the next local midnight through the
// calendar rather than by adding 24h, so a daylight-saving change does not
// shift the search off the hour.
func TestNextIsStableAcrossADaylightSavingChange(t *testing.T) {
	sydney, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	// Sydney springs forward at 02:00 on 2026-10-04.
	from := time.Date(2026, 10, 2, 12, 0, 0, 0, sydney)
	got := mustParse(t, "0 9 * * 1").Next(from) // the following Monday
	want := time.Date(2026, 10, 5, 9, 0, 0, 0, sydney)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
