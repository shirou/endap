package cli

import (
	"crypto/sha256"

	"github.com/shirou/endap/internal/record"
)

// dupKey identifies an exact duplicate: the spec's (ts, cmd, host).
func dupKey(r *record.Record) string {
	return string(record.DuplicateKey(r))
}

// rawKey identifies a line the parser is not allowed to interpret.
//
// Records from a newer endap are passed through untouched, which means they are
// not in the (ts, cmd, host) map and would otherwise pile up: every sync round
// hands the same record back and nothing removes the copy. Hashing the raw
// bytes deduplicates them without parsing them.
func rawKey(line []byte) [sha256.Size]byte {
	return sha256.Sum256(line)
}

// dedupe tracks both kinds of key for one rewrite.
type dedupe struct {
	records map[string]bool
	raw     map[[sha256.Size]byte]bool
}

func newDedupe() *dedupe {
	return &dedupe{records: map[string]bool{}, raw: map[[sha256.Size]byte]bool{}}
}

// seenRecord reports whether an identical record has already been kept.
func (d *dedupe) seenRecord(r *record.Record) bool {
	k := dupKey(r)
	if d.records[k] {
		return true
	}
	d.records[k] = true
	return false
}

// seenRaw reports whether an identical raw line has already been kept.
func (d *dedupe) seenRaw(line []byte) bool {
	k := rawKey(line)
	if d.raw[k] {
		return true
	}
	d.raw[k] = true
	return false
}

// counts is what a rewrite reports back to the user.
type counts struct {
	kept       int
	duplicates int
	ignored    int
	corrupt    int
	unknown    int
	matched    int
}
