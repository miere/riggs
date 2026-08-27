package blockkit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMarkersAreTextPresentation(t *testing.T) {
	markers := map[string]string{
		"MarkerRunning": MarkerRunning,
		"MarkerDone":    MarkerDone,
		"MarkerFailed":  MarkerFailed,
		"MarkerWarning": MarkerWarning,
		"MarkerOpen":    MarkerOpen,
		"MarkerAsk":     MarkerAsk,
	}
	for name, glyph := range markers {
		if r, what, isEmoji := ContainsEmoji(glyph); isEmoji {
			t.Errorf("%s is %U (%s), which Slack renders as an image", name, r, what)
		}
	}
}

// The two that were actually shipped, and the near-misses beside them.
func TestEmojiPresentationCatchesTheUsualSuspects(t *testing.T) {
	for _, r := range []rune{'⏺', '⚠', '✅', '❌', '▶', '⭐', '❗', '✔', '🎉'} {
		if _, _, isEmoji := ContainsEmoji(string(r)); !isEmoji {
			t.Errorf("%U was not recognised as emoji-presentation", r)
		}
	}
	// And the ones that are genuinely fine, or the rule would be useless.
	for _, r := range []rune{'✓', '✗', '⧉', '✎', '•', '›', '—', '…'} {
		if _, what, isEmoji := ContainsEmoji(string(r)); isEmoji {
			t.Errorf("%U was wrongly flagged as %s", r, what)
		}
	}
}

// No string literal anywhere in internal/ may contain an emoji-presentation
// character.
//
// This scans the source rather than the rendered output because that is where
// the mistake lives and where it is invisible: `⏺` and `✓` look equally plain in
// an editor, and only one of them turns into a colour image in Slack. Setting a
// text object's `emoji` flag to false does NOT prevent it — that flag governs
// `:shortcode:` parsing and nothing else, which is how `⏺` shipped on a block
// that already had `emoji: false`.
//
// go/parser is used rather than a grep so comments are excluded for free. The
// prose above is allowed to name the characters it is warning about.
func TestNoEmojiInStringLiterals(t *testing.T) {
	root := filepath.Join("..") // internal/

	var offences []string
	fset := token.NewFileSet()

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if r, what, isEmoji := ContainsEmoji(value); isEmoji {
				offences = append(offences, fset.Position(lit.Pos()).String()+
					": "+strconv.QuoteRune(r)+" ("+what+")")
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	for _, o := range offences {
		t.Errorf("emoji in a string literal — Slack will render it as an image:\n  %s", o)
	}
}
