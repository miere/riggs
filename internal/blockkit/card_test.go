package blockkit

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// semantic compares two JSON documents by structure, ignoring key order —
// which is what Slack cares about.
func semantic(t *testing.T, got []any, want string) {
	t.Helper()
	gotBytes, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var g, w any
	if err := json.Unmarshal(gotBytes, &g); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("card mismatch\n got: %s\nwant: %s", gotBytes, want)
	}
}

// The live PR card, as the Python renders it today. This is the shape the
// cutover must reproduce: same block types, same block_ids, same action_ids.
func TestReviewablePullRequestCard(t *testing.T) {
	card := Card{
		Title:          "Fix the resolver",
		Subtitle:       "acme/monolith#20069",
		IconURL:        "https://example.test/gh.png",
		IconAlt:        "GitHub",
		Body:           "Repoints the owner link.",
		BodyBlockID:    "pr_summary",
		ActionsBlockID: "acme/monolith#20069",
		Actions: []Element{
			Button{ActionID: "approve_only", Text: "Approve", Value: "acme/monolith#20069", Primary: true},
			LinkButton{Text: "Open in Browser", URL: "https://github.com/x/y/pull/1"},
		},
	}

	semantic(t, card.Blocks(), `[{
      "type":"container","width":"wide",
      "title":{"type":"plain_text","text":"Fix the resolver"},
      "subtitle":{"type":"plain_text","text":"acme/monolith#20069"},
      "icon":{"type":"image","image_url":"https://example.test/gh.png","alt_text":"GitHub"},
      "is_collapsible":true,"default_collapsed":false,
      "child_blocks":[
        {"type":"section","block_id":"pr_summary","text":{"type":"mrkdwn","text":"Repoints the owner link."}},
        {"type":"divider"},
        {"type":"actions","block_id":"acme/monolith#20069","elements":[
          {"type":"button","action_id":"approve_only","style":"primary","value":"acme/monolith#20069",
           "text":{"type":"plain_text","text":"Approve","emoji":true}},
          {"type":"button","url":"https://github.com/x/y/pull/1",
           "text":{"type":"plain_text","text":"Open in Browser","emoji":true}}
        ]}
      ]}]`)
}

// A not-reviewable PR keeps its summary and swaps the actions row for a single
// context line.
func TestCollapsedPullRequestCardKeepsSummary(t *testing.T) {
	card := Card{
		Title: "T", Subtitle: "o/r#2", Body: "S", BodyBlockID: "pr_summary",
		Collapsed: true, Context: "Merged at: May 14, 2026 at 3:42 PM",
	}
	semantic(t, card.Blocks(), `[{
      "type":"container","width":"wide",
      "title":{"type":"plain_text","text":"T"},
      "subtitle":{"type":"plain_text","text":"o/r#2"},
      "is_collapsible":true,"default_collapsed":true,
      "child_blocks":[
        {"type":"section","block_id":"pr_summary","text":{"type":"mrkdwn","text":"S"}},
        {"type":"divider"},
        {"type":"context","elements":[{"type":"mrkdwn","text":"Merged at: May 14, 2026 at 3:42 PM"}]}
      ]}]`)
}

// A resolved ticket card drops the description entirely — the QCT renderer
// emits no section block once the task is claimed. An empty Body must
// therefore render no section, not an empty one.
func TestResolvedTicketCardHasNoSection(t *testing.T) {
	card := Card{
		Title: "Add a health probe", Subtitle: "NYX-1234",
		Collapsed: true, Context: "Assigned to: Miere - Last updated: May 14, 2026 at 3:42 PM",
	}
	semantic(t, card.Blocks(), `[{
      "type":"container","width":"wide",
      "title":{"type":"plain_text","text":"Add a health probe"},
      "subtitle":{"type":"plain_text","text":"NYX-1234"},
      "is_collapsible":true,"default_collapsed":true,
      "child_blocks":[
        {"type":"divider"},
        {"type":"context","elements":[{"type":"mrkdwn","text":"Assigned to: Miere - Last updated: May 14, 2026 at 3:42 PM"}]}
      ]}]`)
}

// The fingerprint is the ledger's "did this change?" test, so an unchanged
// card must hash identically every time. Go maps would not guarantee this;
// the ordered structs are what make it hold.
func TestFingerprintIsStable(t *testing.T) {
	card := Card{
		Title: "T", Subtitle: "S", Body: "B", ActionsBlockID: "id",
		Actions: []Element{
			Button{ActionID: "a", Text: "A", Value: "v", Primary: true},
			LinkButton{ActionID: "o", Text: "Open", URL: "https://example.test"},
		},
	}
	first := card.Fingerprint()
	if first == "" {
		t.Fatal("Fingerprint returned empty")
	}
	for i := 0; i < 50; i++ {
		if got := card.Fingerprint(); got != first {
			t.Fatalf("Fingerprint is unstable: %s != %s (iteration %d)", got, first, i)
		}
	}
}

func TestFingerprintTracksEveryVisibleChange(t *testing.T) {
	base := Card{Title: "T", Subtitle: "S", Body: "B", Context: "C", Collapsed: false}
	for _, tc := range []struct {
		name string
		card Card
	}{
		{"body", func() Card { c := base; c.Body = "different"; return c }()},
		{"title", func() Card { c := base; c.Title = "different"; return c }()},
		{"context", func() Card { c := base; c.Context = "different"; return c }()},
		{"collapsed", func() Card { c := base; c.Collapsed = true; return c }()},
		{"actions appear", func() Card {
			c := base
			c.Actions = []Element{Button{ActionID: "a", Text: "A", Value: "v"}}
			return c
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.card.Fingerprint() == base.Fingerprint() {
				t.Errorf("changing %s did not change the fingerprint", tc.name)
			}
		})
	}
}

// An actions row wins over a context line: a live card shows buttons, not a
// status label, even if both are set.
func TestActionsSupersedeContext(t *testing.T) {
	card := Card{Title: "T", Subtitle: "S", Context: "should not appear",
		Actions: []Element{Button{ActionID: "a", Text: "A", Value: "v"}}}
	encoded, _ := json.Marshal(card.Blocks())
	if got := string(encoded); strings.Contains(got, "should not appear") {
		t.Errorf("context rendered alongside actions: %s", got)
	}
}

// Slack rejects the whole message — not just the block — when a section runs
// past 3,000 characters, so an uncapped body is a card that never posts and a
// click that looks to the user like nothing happened.
func TestLongBodyIsCappedUnderSlacksSectionLimit(t *testing.T) {
	card := Card{Title: "T", Subtitle: "S", BodyBlockID: "pr_summary",
		Body: strings.Repeat("x", 9000)}

	encoded, err := json.Marshal(card.Blocks())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := strings.Repeat("x", bodyLimit) + "…"
	if !strings.Contains(string(encoded), body) {
		t.Errorf("body was not cut to %d runes + ellipsis", bodyLimit)
	}
	if n := len([]rune(body)); n >= 3000 {
		t.Errorf("cut body is %d runes, still at or over Slack's limit", n)
	}
}

// A body already inside the limit is passed through untouched — the cap is a
// backstop, and an ellipsis on a short card would be a lie.
func TestShortBodyIsUnchanged(t *testing.T) {
	card := Card{Title: "T", Subtitle: "S", Body: "Repoints the owner link."}
	if got := string(mustJSON(t, card.Blocks())); !strings.Contains(got, "Repoints the owner link.\"") {
		t.Errorf("short body was altered: %s", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestTextAndContextBlocks(t *testing.T) {
	semantic(t, TextBlocks("<@U1> ready for review"),
		`[{"type":"section","text":{"type":"mrkdwn","text":"<@U1> ready for review"}}]`)
	semantic(t, ContextBlocks("✓ Approved o/r#1."),
		`[{"type":"context","elements":[{"type":"plain_text","text":"✓ Approved o/r#1.","emoji":false}]}]`)
}
