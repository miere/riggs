package ai

import "strings"

// Text renders the instruction handed to the harness.
//
// `{ref}` — with `{key}` as its alias, because a ticket is not called a ref by
// anybody — and `{url}` are replaced with the item's reference and its browser
// URL, so a configured wording can put them wherever it likes.
//
// The wording is configuration. The SUBJECT is not: whatever the prompt did,
// the reference and the URL end up in the result, appended on their own line if
// the wording left them out. That is the same guarantee internal/ask makes
// about its two mentions, made here for the same reason — a prompt edited to
// drop the reference fails SILENTLY. The harness still starts, still runs, and
// still reports success, having reviewed whatever it happened to find in the
// working directory.
//
// It names no tool and claims no authorship, on the same rule as everything
// else Riggs sends: what the harness does next is submitted under the admin's
// own credentials and reads as their own work, because their click is what
// produced it.
func Text(prompt, ref, url string) string {
	text := strings.NewReplacer(
		"{ref}", ref,
		"{key}", ref,
		"{url}", url,
	).Replace(strings.TrimSpace(prompt))

	var missing []string
	if ref != "" && !strings.Contains(text, ref) {
		missing = append(missing, ref)
	}
	if url != "" && !strings.Contains(text, url) {
		missing = append(missing, url)
	}
	if len(missing) == 0 {
		return text
	}
	// Labelled rather than appended bare. The harness is reading this as an
	// instruction, and a URL dangling off the end of a sentence is ambiguous in
	// a way "Subject: …" is not.
	return strings.TrimSpace(text + "\n\nSubject: " + strings.Join(missing, " "))
}
