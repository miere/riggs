package slack

import "errors"

// reportedError marks an error whose cause has already been shown to the user.
//
// The daemon reports a failed click to whoever made it, so that a broken button
// stops looking like an ignored one. Several handlers get there first and say it
// better: they narrate into the thread the click came from, or into the Home
// tab, naming what was being attempted — which a generic dispatcher cannot do.
// This is how they say so, and it is why the same failure does not arrive twice.
//
// It lives here rather than in any one of them because more than one package
// needs it, and a marker duplicated per package is a marker that will drift.
// The contract is the method, not this type: internal/daemon asserts on
// `interface{ Reported() bool }` rather than importing anything, so the dispatch
// layer stays free of the domain and a package with its own error type can opt
// in without reaching for this one.
//
// Unwrap keeps errors.Is and errors.As working through the marker, because the
// wrapped error is still the real one.
type reportedError struct{ err error }

func (e reportedError) Error() string  { return e.err.Error() }
func (e reportedError) Unwrap() error  { return e.err }
func (e reportedError) Reported() bool { return true }

// Reported marks err as already shown to the user.
//
// Only for a caller that has actually shown it, and only when that succeeded: a
// message marked reported is one the daemon will stay quiet about, so marking
// an announcement that failed to post is how a failure becomes silent again.
//
// A nil error stays nil. "Nothing went wrong" is not something to report.
func Reported(err error) error {
	if err == nil {
		return nil
	}
	return reportedError{err: err}
}

// WasReported reports whether err has been shown to the user by whoever
// produced it. It is the same question internal/daemon asks, exported for
// callers that need to make the same decision.
func WasReported(err error) bool {
	var r interface{ Reported() bool }
	return errors.As(err, &r) && r.Reported()
}
