package pullrequest

// What is left of the per-pull-request card.
//
// The card loop is gone — §12c retired its job, this phase retired its tool —
// and `Card`, `FallbackText` and `TagText` went with it. Two declarations
// outlived them, both because something else took a dependency on them:
//
//   - iconURL is the GitHub glyph, now worn by the ask-review card.
//   - Key is the ledger key, still read by Approver.thread to find the message
//     an approval should narrate under.
//
// The renderer was deliberately kept when the job was retired, on the grounds
// that "the shape is about to be reused for something else". It was — as
// blockkit.Card, by the ask cards on both sides — so the shape survives and
// this loop's own use of it does not.

// iconURL is the GitHub avatar shown on a pull-request card.
const iconURL = "https://avatars.slack-edge.com/2020-11-25/1527503386626_319578f21381f9641cd8_512.png"

// Key is the ledger key for a pull request's card.
func Key(ref string) string { return "git.pr:" + ref }
