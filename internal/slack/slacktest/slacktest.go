// Package slacktest provides an in-memory slack.Poster for tests, so no test
// makes a live call. It is a sibling of the domain package by the blueprint's
// convention: when several packages share a seam, the fake is promoted rather
// than re-implemented in every _test.go.
package slacktest

import (
	"context"
	"fmt"

	"github.com/miere/riggs-mcp/internal/slack"
)

// Call records one interaction with the fake.
type Call struct {
	// Kind is "post", "update" or "delete".
	Kind   string
	Target slack.Target
	// Ref is the message being updated or deleted; zero for a post.
	Ref slack.Ref
	Msg slack.Message
}

// Fake records calls and returns scripted results.
type Fake struct {
	Calls []Call

	// PostErr, when set, fails every Post.
	PostErr error
	// UpdateErr, when set, fails every Update — set it to
	// slack.ErrMessageNotFound to exercise the re-post path.
	UpdateErr error
	// DeleteErr, when set, fails every Delete.
	DeleteErr error

	seq int
}

// New returns an empty Fake.
func New() *Fake { return &Fake{} }

// Post records the call and returns a synthetic Ref with a fresh ts.
func (f *Fake) Post(_ context.Context, target slack.Target, msg slack.Message) (slack.Ref, error) {
	f.Calls = append(f.Calls, Call{Kind: "post", Target: target, Msg: msg})
	if f.PostErr != nil {
		return slack.Ref{}, f.PostErr
	}
	f.seq++
	channel := target.Channel
	if channel == "" {
		// Mirror the live client: a DM target resolves to a conversation id.
		channel = "D" + target.AdminUserID
	}
	return slack.Ref{Channel: channel, TS: fmt.Sprintf("17000000.%06d", f.seq)}, nil
}

// Update records the call and returns UpdateErr.
func (f *Fake) Update(_ context.Context, target slack.Target, ref slack.Ref, msg slack.Message) error {
	f.Calls = append(f.Calls, Call{Kind: "update", Target: target, Ref: ref, Msg: msg})
	return f.UpdateErr
}

// Delete records the call and returns DeleteErr.
func (f *Fake) Delete(_ context.Context, target slack.Target, ref slack.Ref) error {
	f.Calls = append(f.Calls, Call{Kind: "delete", Target: target, Ref: ref})
	return f.DeleteErr
}

// Deleted returns the refs passed to Delete, in order.
func (f *Fake) Deleted() []slack.Ref {
	var out []slack.Ref
	for _, c := range f.Calls {
		if c.Kind == "delete" {
			out = append(out, c.Ref)
		}
	}
	return out
}

// Kinds returns the sequence of call kinds, which is usually what a test wants
// to assert on.
func (f *Fake) Kinds() []string {
	out := make([]string, 0, len(f.Calls))
	for _, c := range f.Calls {
		out = append(out, c.Kind)
	}
	return out
}

// Posts returns only the post calls.
func (f *Fake) Posts() []Call {
	var out []Call
	for _, c := range f.Calls {
		if c.Kind == "post" {
			out = append(out, c)
		}
	}
	return out
}

// Reset forgets recorded calls, keeping the scripted errors.
func (f *Fake) Reset() { f.Calls = nil }
