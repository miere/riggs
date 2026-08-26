package blockkit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// fingerprint is a stable digest of a rendered block array. The ledger compares
// it to decide whether a Slack update is needed at all, so two renders of
// unchanged content must produce identical bytes.
//
// This is the one piece the container card (card.go) and the bulk digest
// (bulk.go) genuinely share: the rule is the same for both, and it holds only
// because every block in either tree is an ordered struct rather than a map.
//
// An encoding failure degrades to a value that never matches, rather than
// panicking inside a scheduled job. Every type in either tree is a plain struct
// of strings, bools and slices, so it cannot happen.
func fingerprint(blocks []any) string {
	b, err := json.Marshal(blocks)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
