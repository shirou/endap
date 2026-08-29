package store

import (
	"bytes"
	"errors"
	"os"

	"github.com/shirou/endap/internal/record"
)

// Stats summarises what a pass over the log found.
type Stats struct {
	Lines   int // physical lines seen
	Records int // parsed records at a known version
	Corrupt int // unparseable lines, or v missing / <= 0
	Unknown int // records from a newer endap (v > record.Version)
}

// Kind classifies a line for the read and rewrite paths.
type Kind int

const (
	// KindRecord is a line that parsed at a known schema version.
	KindRecord Kind = iota
	// KindUnknown is a well-formed record from a newer endap. The read path
	// skips it; the rewrite path passes the original bytes through untouched,
	// because a sync partner running a newer version is the normal state of a
	// multi-host setup, not an error (D6).
	//
	// This holds only while later schema versions add fields. A future version
	// that changes the type of an existing field, or stops being a flat object,
	// fails to decode here and lands in KindCorrupt instead, which sends it to
	// the .rej file rather than passing it through. Schema changes have to stay
	// additive for the guarantee to mean anything.
	KindUnknown
	// KindCorrupt is a line that did not parse, or whose v is missing or <= 0.
	KindCorrupt
)

// Classify parses one line and says how the caller should treat it.
func Classify(line []byte, dst *record.Record) Kind {
	if err := record.Decode(line, dst); err != nil {
		return KindCorrupt
	}
	switch {
	case dst.V <= 0:
		// A missing or zero v means the line is not an endap record even if it
		// happens to be valid JSON.
		return KindCorrupt
	case dst.V > record.Version:
		return KindUnknown
	default:
		return KindRecord
	}
}

// SplitLines splits a log buffer into physical lines, dropping the newlines. A
// trailing fragment with no newline is returned as its own line so that a
// partial write shows up as one corrupt line rather than disappearing.
func SplitLines(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	n := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		n++
	}
	out := make([][]byte, 0, n)
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			out = append(out, data)
			break
		}
		out = append(out, data[:i])
		data = data[i+1:]
	}
	return out
}

// ReadFile returns the whole log. A missing file is not an error: it just means
// nothing has been recorded yet.
func ReadFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

// Walk parses every line of data and calls fn for each usable record.
//
// The record passed to fn is reused between calls, so fn must copy whatever it
// keeps. That is what lets a full pass over the log stay allocation-light: the
// only lasting allocations are the strings the caller decides to retain.
func Walk(data []byte, fn func(r *record.Record)) Stats {
	var st Stats
	var rec record.Record
	for _, line := range SplitLines(data) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		st.Lines++
		switch Classify(line, &rec) {
		case KindRecord:
			st.Records++
			if fn != nil {
				fn(&rec)
			}
		case KindUnknown:
			st.Unknown++
		case KindCorrupt:
			st.Corrupt++
		}
	}
	return st
}

// WalkFile is Walk over a file on disk.
func WalkFile(path string, fn func(r *record.Record)) (Stats, error) {
	data, err := ReadFile(path)
	if err != nil {
		return Stats{}, err
	}
	return Walk(data, fn), nil
}
