package approve

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/pullrequest"
	"github.com/miere/riggs-mcp/internal/slack"
)

// recorder captures what the tool asked the approver to do.
type recorder struct {
	ref    string
	merge  bool
	target slack.Target
	thread string
	calls  int
	dry    bool
}

func (r *recorder) DryRun(_ context.Context, ref string, merge bool, target slack.Target, thread string) (pullrequest.ApproveResult, error) {
	r.dry = true
	return r.Run(context.Background(), ref, merge, target, thread)
}

func (r *recorder) Run(_ context.Context, ref string, merge bool, target slack.Target, thread string) (pullrequest.ApproveResult, error) {
	r.calls++
	r.ref, r.merge, r.target, r.thread = ref, merge, target, thread
	return pullrequest.ApproveResult{Ref: ref, Approved: true, Merged: merge, Message: "ok"}, nil
}

func tools(t *testing.T) (*Tool, *Tool, *recorder) {
	t.Helper()
	cfg := &config.Config{
		Path:  "<test>",
		Admin: config.Admin{SlackUserID: "U1"},
		Slack: config.Slack{Profiles: map[string]config.Profile{
			"default": {BotToken: "xoxb"},
		}},
	}
	rec := &recorder{}
	factory := func(context.Context) (Approver, io.Closer, error) { return rec, nil, nil }
	resolver := slack.NewResolver(cfg)
	return New(resolver, factory), NewMerge(resolver, factory), rec
}

// The two operations are separate registrations so a workflow rule names
// exactly one.
func TestNamesAreDistinct(t *testing.T) {
	plain, merge, _ := tools(t)
	if plain.Name() != "git.pr.approve" {
		t.Errorf("name = %q", plain.Name())
	}
	if merge.Name() != "git.pr.approve-merge" {
		t.Errorf("name = %q", merge.Name())
	}
	if plain.PrimaryArg() != "pr" {
		t.Errorf("PrimaryArg = %q, want pr", plain.PrimaryArg())
	}
}

func TestApproveDoesNotMerge(t *testing.T) {
	plain, _, rec := tools(t)

	if _, err := plain.Invoke(context.Background(), map[string]any{
		"pr": "o/r#1", "slack_channel": "C1", "thread": "1700.1",
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if rec.ref != "o/r#1" || rec.merge {
		t.Errorf("asked for ref=%q merge=%v; want the plain approval", rec.ref, rec.merge)
	}
	if rec.thread != "1700.1" {
		t.Errorf("thread = %q, want the clicked message", rec.thread)
	}
}

func TestApproveMergeMerges(t *testing.T) {
	_, merge, rec := tools(t)

	if _, err := merge.Invoke(context.Background(), map[string]any{
		"pr": "o/r#1", "slack_channel": "C1",
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !rec.merge {
		t.Error("approve-merge did not request a merge")
	}
}

// The merge flag is fixed at construction. A payload must not be able to turn
// an approval into a merge.
func TestMergeCannotBeSetFromArguments(t *testing.T) {
	plain, _, rec := tools(t)

	if _, err := plain.Invoke(context.Background(), map[string]any{
		"pr": "o/r#1", "slack_channel": "C1", "merge": true, "action_id": "approve_merge",
	}); err != nil {
		// Unknown properties are ignored by the tool; the CLI rejects them
		// earlier against the schema. Either way it must not merge.
		t.Fatalf("Invoke: %v", err)
	}
	if rec.merge {
		t.Error("an argument turned a plain approval into a merge")
	}
}

// Approving the wrong pull request cannot be undone from Slack, so a
// malformed ref fails before anything is opened or called.
func TestMalformedRefFailsBeforeTouchingGitHub(t *testing.T) {
	plain, _, rec := tools(t)

	for _, bad := range []string{"", "no-hash", "o/r#abc"} {
		if _, err := plain.Invoke(context.Background(), map[string]any{"pr": bad}); err == nil {
			t.Errorf("Invoke(%q) = nil error", bad)
		}
	}
	if rec.calls != 0 {
		t.Errorf("approver ran %d times for malformed refs", rec.calls)
	}
}

// The approval must still happen when there is no card and no channel — it
// simply goes unannounced rather than being refused.
func TestUnresolvableTargetStillApproves(t *testing.T) {
	cfg := &config.Config{Path: "<test>"} // no profiles, no admin
	rec := &recorder{}
	tool := New(slack.NewResolver(cfg), func(context.Context) (Approver, io.Closer, error) {
		return rec, nil, nil
	})

	if _, err := tool.Invoke(context.Background(), map[string]any{"pr": "o/r#1"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if rec.calls != 1 {
		t.Error("the approval was skipped because Slack could not be resolved")
	}
}

// ...but an explicit thread means the caller expects a reply, so an
// unresolvable target is a real error there.
func TestExplicitThreadRequiresAResolvableTarget(t *testing.T) {
	cfg := &config.Config{Path: "<test>"}
	rec := &recorder{}
	tool := New(slack.NewResolver(cfg), func(context.Context) (Approver, io.Closer, error) {
		return rec, nil, nil
	})

	_, err := tool.Invoke(context.Background(), map[string]any{"pr": "o/r#1", "thread": "1700.1"})
	if err == nil {
		t.Fatal("Invoke = nil error with a thread but no deliverable target")
	}
	if rec.calls != 0 {
		t.Error("ran the approval despite failing to resolve the reply target")
	}
}

func TestSchemaDeclaresTheClickedMessageFields(t *testing.T) {
	plain, _, _ := tools(t)
	for _, want := range []string{"pr", "thread", "slack_profile", "slack_channel"} {
		if _, ok := plain.InputSchema().Properties[want]; !ok {
			t.Errorf("schema is missing %q", want)
		}
	}
	if req := plain.InputSchema().Required; len(req) != 1 || req[0] != "pr" {
		t.Errorf("required = %v, want just pr", req)
	}
}

func TestDescriptionsDistinguishTheOperations(t *testing.T) {
	plain, merge, _ := tools(t)
	if strings.Contains(plain.Description(), "merge") {
		t.Errorf("plain description mentions merging: %q", plain.Description())
	}
	if !strings.Contains(merge.Description(), "rebase") {
		t.Errorf("merge description does not name the rebase: %q", merge.Description())
	}
}
