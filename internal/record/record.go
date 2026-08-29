// Package record defines the endap history record and its JSONL encoding.
package record

import "strconv"

// Version is the schema version written into every new record.
//
// Records with a larger v are from a newer endap and are skipped by the read
// path (D6). Records with v <= 0 are treated as corrupt.
const Version = 1

// Record is one shell history entry.
//
// The optional numeric fields carry an explicit presence flag rather than being
// pointers: dur=0 and exit=0 are both meaningful values, and the read path is
// the hot path, so decoding must not allocate per record. The pointer-shaped
// struct that gives encoding/json its omitempty behaviour lives in encode.go.
type Record struct {
	V       int
	ID      string
	TS      int64 // start time, Unix milliseconds
	Dur     int64 // duration in milliseconds
	HasDur  bool
	Cmd     string
	Cwd     string
	Host    string
	Sess    string
	Exit    int
	HasExit bool
	Sh      string
}

// SetDur records a known duration in milliseconds.
func (r *Record) SetDur(ms int64) {
	r.Dur, r.HasDur = ms, true
}

// SetExit records a known exit status.
func (r *Record) SetExit(code int) {
	r.Exit, r.HasExit = code, true
}

// DuplicateKey returns the identity of a record for exact-duplicate removal:
// the spec's (ts, cmd, host), laid out as ts, host, cmd separated by NUL.
//
// cmd goes last because it is the only one of the three that can contain a NUL
// byte: a command read from stdin can hold one, and "\u0000" decodes back to a
// bare 0x00. ts is decimal digits and host comes from the hostname or
// ENDAP_HOST, so neither can, which leaves the two separators unambiguous and
// anything after the second one as the command. Reordering these fields would
// reintroduce the collision.
func DuplicateKey(r *Record) []byte {
	b := make([]byte, 0, len(r.Cmd)+len(r.Host)+24)
	b = strconv.AppendInt(b, r.TS, 10)
	b = append(b, 0)
	b = append(b, r.Host...)
	b = append(b, 0)
	b = append(b, r.Cmd...)
	return b
}
