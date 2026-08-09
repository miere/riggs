package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/miere/riggs-mcp/internal/tools"
)

type fakeTool struct {
	name   string
	schema *jsonschema.Schema
	result any
	err    error
	got    map[string]any
}

func (f *fakeTool) Name() string                    { return f.name }
func (f *fakeTool) Description() string             { return "fake" }
func (f *fakeTool) InputSchema() *jsonschema.Schema { return f.schema }
func (f *fakeTool) Invoke(_ context.Context, a map[string]any) (any, error) {
	f.got = a
	return f.result, f.err
}

func TestDecodeArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want int
	}{
		{"empty", "", 0},
		{"null", "null", 0},
		{"object", `{"pr":"owner/repo#1"}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeArgs(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("decodeArgs: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("decodeArgs(%q) = %v, want %d entries", tc.raw, got, tc.want)
			}
		})
	}
}

func TestDecodeArgsRejectsGarbage(t *testing.T) {
	if _, err := decodeArgs(json.RawMessage("[1,2]")); err == nil {
		t.Fatal("decodeArgs(array) = nil error, want a failure")
	}
}

// A plain string passes through so trivial tools stay uncluttered; everything
// else is JSON, which is the same shape the CLI's --json-output emits.
func TestRenderJSON(t *testing.T) {
	if got, _ := renderJSON("pong"); got != "pong" {
		t.Errorf("renderJSON(string) = %q, want it passed through", got)
	}
	got, err := renderJSON(struct {
		A int `json:"a"`
	}{1})
	if err != nil {
		t.Fatalf("renderJSON: %v", err)
	}
	if got != `{"a":1}` {
		t.Errorf("renderJSON(struct) = %q, want JSON", got)
	}
}

// A tool that takes no parameters must still publish a valid object schema:
// the SDK requires one.
func TestNilSchemaBecomesEmptyObject(t *testing.T) {
	s := emptyObjectSchema()
	if s.Type != "object" {
		t.Errorf("emptyObjectSchema().Type = %q, want object", s.Type)
	}
}

func TestServerRegistersEveryTool(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "ping"})
	reg.Register(&fakeTool{name: "git.pr.approve", schema: &jsonschema.Schema{Type: "object"}})

	srv := New(reg).Server()

	// The SDK exposes registered tools over a connected session; drive it
	// in-memory rather than over stdio.
	ctx := context.Background()
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	// Published under the MCP spelling, not the registry key.
	for _, want := range []string{"ping", "git_pr_approve"} {
		if !names[want] {
			t.Errorf("tool %q not published; got %v", want, names)
		}
	}
	if names["git.pr.approve"] {
		t.Error("the dotted registry name leaked to the MCP boundary")
	}
}

// Every dot and hyphen becomes an underscore, matching Murtaugh's convention.
// Registry keys and CLI spellings are untouched by this.
func TestToolName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"ping", "ping"},
		{"jira.tickets", "jira_tickets"},
		{"slack.send-msg", "slack_send_msg"},
		{"git.pr.fetch-reviews", "git_pr_fetch_reviews"},
		{"git.pr.approve-merge", "git_pr_approve_merge"},
	} {
		if got := ToolName(tc.in); got != tc.want {
			t.Errorf("ToolName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A normalised name must actually dispatch — publishing it is not enough.
func TestCallByNormalisedName(t *testing.T) {
	fake := &fakeTool{name: "slack.send-msg", schema: &jsonschema.Schema{Type: "object"}, result: "sent"}
	reg := tools.NewRegistry()
	reg.Register(fake)

	ctx := context.Background()
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	if _, err := New(reg).Server().Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "slack_send_msg"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("calling by the normalised name failed: %+v", res.Content)
	}
	text, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok || text.Text != "sent" {
		t.Errorf("content = %+v, want the tool's own result", res.Content[0])
	}
}

// Normalisation collapses two characters into one, so a collision is possible
// in principle. It must be refused, not left to silently shadow a tool.
func TestCollidingNamesPanic(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "a.b-c"})
	reg.Register(&fakeTool{name: "a-b.c"})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Server() did not panic on colliding MCP names")
		}
		if !strings.Contains(fmt.Sprint(r), "a_b_c") {
			t.Errorf("panic = %v, want it to name the collision", r)
		}
	}()
	New(reg).Server()
}

// Tool failures surface as IsError results, not transport errors, per the MCP
// convention.
func TestToolErrorIsAnErrorResult(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "boom", err: context.DeadlineExceeded})

	ctx := context.Background()
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	if _, err := New(reg).Server().Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "boom"})
	if err != nil {
		t.Fatalf("CallTool returned a transport error: %v", err)
	}
	if !res.IsError {
		t.Error("CallTool result IsError = false, want the tool failure surfaced")
	}
}
