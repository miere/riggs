package blockkit

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleDigest() Digest {
	return Digest{
		Title:    "GitHub - Pull Requests",
		Subtitle: "You have been assigned some juicy code reviews.",
		IconURL:  "https://example.test/icon.png",
		IconAlt:  "Icon",
		Rows: []Row{{
			BlockID:  "acme/monolith#20534",
			Title:    "TFL-3338: derive business contact type and name",
			Meta:     "_acme/monolith#20534_ by `@sam`",
			ActionID: "pr_bulk_overflow",
			Options: []MenuOption{
				{Text: "⧉  Open on Browser", Value: "open_browser", URL: "https://github.test/pr/1"},
				{Text: "✎  Ask for Code Review", Value: "ask_review"},
			},
		}},
	}
}

// decode renders and re-parses, so assertions are against the wire shape rather
// than the Go structs.
func decode(t *testing.T, d Digest) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(d.Blocks())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestDigestRendersACardHeader(t *testing.T) {
	blocks := decode(t, sampleDigest())
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want header + 1 row", len(blocks))
	}

	header := blocks[0]
	if header["type"] != "card" {
		t.Errorf("header type = %v, want card", header["type"])
	}
	title := header["title"].(map[string]any)
	if title["type"] != "mrkdwn" || title["text"] != "GitHub - Pull Requests" {
		t.Errorf("header title = %v", title)
	}
	if _, ok := title["verbatim"]; !ok {
		t.Error("header title does not declare verbatim")
	}
	icon := header["icon"].(map[string]any)
	if icon["image_url"] != "https://example.test/icon.png" || icon["alt_text"] != "Icon" {
		t.Errorf("header icon = %v", icon)
	}
}

// The block_id is the only per-row field an overflow click carries back, so a
// row that loses it becomes unactionable.
func TestRowCarriesItsIdentityInTheBlockID(t *testing.T) {
	row := decode(t, sampleDigest())[1]
	if row["type"] != "section" {
		t.Fatalf("row type = %v, want section", row["type"])
	}
	if row["block_id"] != "acme/monolith#20534" {
		t.Fatalf("row block_id = %v", row["block_id"])
	}
}

func TestRowMenuIsAnOverflowAccessory(t *testing.T) {
	row := decode(t, sampleDigest())[1]
	acc, ok := row["accessory"].(map[string]any)
	if !ok {
		t.Fatal("row has no accessory")
	}
	if acc["type"] != "overflow" || acc["action_id"] != "pr_bulk_overflow" {
		t.Fatalf("accessory = %v", acc)
	}

	opts := acc["options"].([]any)
	if len(opts) != 2 {
		t.Fatalf("got %d options, want 2", len(opts))
	}

	first := opts[0].(map[string]any)
	if first["value"] != "open_browser" || first["url"] != "https://github.test/pr/1" {
		t.Errorf("link option = %v", first)
	}
	// Labels lead with literal glyphs, so emoji interpretation must be off.
	if text := first["text"].(map[string]any); text["emoji"] != false {
		t.Errorf("option text = %v, want emoji:false", text)
	}
	// An option with no URL must not emit an empty one: Slack rejects it.
	if second := opts[1].(map[string]any); second["url"] != nil {
		t.Errorf("non-link option carries url = %v", second["url"])
	}
}

func TestDoneRowIsStruckThrough(t *testing.T) {
	d := sampleDigest()
	d.Rows[0].Done = true
	d.Rows[0].Options = []MenuOption{{Text: "⧉  Open on Browser", Value: "open_browser", URL: "u"}}

	row := decode(t, d)[1]
	text := row["text"].(map[string]any)["text"].(string)

	if !strings.HasPrefix(text, "~*") {
		t.Errorf("done row title is not struck through: %q", text)
	}
	// Only the title is struck; the reference line stays legible, because it is
	// what you read to find the thing again.
	if strings.Count(text, "~") != 2 {
		t.Errorf("strike-through leaked past the title: %q", text)
	}
	if !strings.Contains(text, "_acme/monolith#20534_") {
		t.Errorf("done row lost its reference line: %q", text)
	}
}

func TestRowWithNoOptionsHasNoMenu(t *testing.T) {
	d := sampleDigest()
	d.Rows[0].Options = nil

	row := decode(t, d)[1]
	if _, present := row["accessory"]; present {
		t.Error("row with no options still rendered an accessory")
	}
}

// A title carrying Slack's markup characters must not be able to re-open the
// row's own bold run and garble every row after it.
func TestRowEscapesMarkupInATitle(t *testing.T) {
	d := sampleDigest()
	d.Rows[0].Title = "fix <script> & >alert<"

	text := decode(t, d)[1]["text"].(map[string]any)["text"].(string)
	for _, bad := range []string{"<script>", " & "} {
		if strings.Contains(text, bad) {
			t.Errorf("unescaped %q in %q", bad, text)
		}
	}
	if !strings.Contains(text, "&lt;script&gt;") || !strings.Contains(text, "&amp;") {
		t.Errorf("escaping did not happen: %q", text)
	}
}

func TestFingerprintTracksContent(t *testing.T) {
	a := sampleDigest()
	if a.Fingerprint() != sampleDigest().Fingerprint() {
		t.Fatal("two renders of the same digest disagree")
	}

	// Every field the reader can see must move the fingerprint, or the ledger
	// will skip an update that mattered.
	for name, mutate := range map[string]func(*Digest){
		"a struck row":     func(d *Digest) { d.Rows[0].Done = true },
		"a changed title":  func(d *Digest) { d.Rows[0].Title = "something else" },
		"a dropped option": func(d *Digest) { d.Rows[0].Options = d.Rows[0].Options[:1] },
		"a new row":        func(d *Digest) { d.Rows = append(d.Rows, Row{BlockID: "o/r#2"}) },
		"a new subtitle":   func(d *Digest) { d.Subtitle = "different" },
	} {
		b := sampleDigest()
		mutate(&b)
		if b.Fingerprint() == a.Fingerprint() {
			t.Errorf("%s did not change the fingerprint", name)
		}
	}
}

// Row order is membership order, and it is what the ledger records.
func TestItemsListsRowsInOrder(t *testing.T) {
	d := sampleDigest()
	d.Rows = append(d.Rows, Row{BlockID: "o/r#2"}, Row{BlockID: "o/r#3"})

	got := d.Items()
	want := []string{"acme/monolith#20534", "o/r#2", "o/r#3"}
	if len(got) != len(want) {
		t.Fatalf("Items() = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Items() = %v, want %v", got, want)
		}
	}
}

func TestEmptyReportsNoRows(t *testing.T) {
	if (Digest{}).Empty() != true {
		t.Error("a digest with no rows is not Empty")
	}
	if sampleDigest().Empty() {
		t.Error("a digest with rows reports Empty")
	}
}

func TestTruncateCountsRunes(t *testing.T) {
	cases := []struct {
		in          string
		limit, keep int
		want        string
	}{
		{"short", 10, 8, "short"},
		{"exactly-ten", 11, 8, "exactly-ten"},
		{"truncate me please", 9, 8, "truncate…"},
		{"日本語のタイトルです", 4, 3, "日本語…"},
		{"trailing space  x", 10, 9, "trailing…"},
	}
	for _, tc := range cases {
		if got := Truncate(tc.in, tc.limit, tc.keep); got != tc.want {
			t.Errorf("Truncate(%q, %d, %d) = %q, want %q", tc.in, tc.limit, tc.keep, got, tc.want)
		}
	}
}

// A row title is left alone up to 50 runes and cut to 47 plus an ellipsis
// beyond that — so the rendered title never exceeds 48.
func TestRowTitleIsCutAtFifty(t *testing.T) {
	exactly50 := strings.Repeat("a", 50)
	d := sampleDigest()
	d.Rows[0].Title = exactly50
	if got := decodeTitle(t, d); got != exactly50 {
		t.Errorf("a 50-rune title was altered: %q", got)
	}

	d.Rows[0].Title = strings.Repeat("b", 51)
	got := decodeTitle(t, d)
	if want := strings.Repeat("b", 47) + "…"; got != want {
		t.Errorf("51-rune title = %q, want %q", got, want)
	}
	if len([]rune(got)) != 48 {
		t.Errorf("rendered title is %d runes, want 48", len([]rune(got)))
	}
}

// decodeTitle pulls the bold title back out of a rendered row.
func decodeTitle(t *testing.T, d Digest) string {
	t.Helper()
	text := decode(t, d)[1]["text"].(map[string]any)["text"].(string)
	line := strings.SplitN(text, "\n", 2)[0]
	return strings.Trim(line, "*~")
}

// The container card and the digest share the fingerprint rule, and must not
// share anything that makes one of them move when the other changes.
func TestCardFingerprintIsUnaffectedByTheDigest(t *testing.T) {
	c := Card{Title: "t", Subtitle: "s", Body: "b"}
	before := c.Fingerprint()
	_ = sampleDigest().Fingerprint()
	if c.Fingerprint() != before {
		t.Fatal("Card.Fingerprint is not stable across a Digest render")
	}
}
