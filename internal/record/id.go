package record

import (
	"crypto/rand"
	"encoding/base32"
)

// crock is Crockford's base32 alphabet. Its symbols are in ASCII ascending
// order, so lexicographic order over the encoded string is the order of the
// underlying bytes, and therefore of the leading timestamp.
var crock = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// NewID returns a 26-character, time-sortable record ID: 48 bits of Unix
// millisecond timestamp followed by 80 bits from crypto/rand.
//
// This is not a canonical ULID string. A ULID is 128 bits written as 26
// Crockford characters, which hold 130 bits, so its first character carries
// only the top 2 bits; RFC 4648 base32 instead packs left to right and leaves
// the slack at the end. endap only needs a key that sorts by time and does not
// collide across hosts, and does not interoperate with external ULID tools, so
// the difference does not matter (D13).
func NewID(tsMillis int64) string {
	var b [16]byte
	b[0] = byte(tsMillis >> 40)
	b[1] = byte(tsMillis >> 32)
	b[2] = byte(tsMillis >> 24)
	b[3] = byte(tsMillis >> 16)
	b[4] = byte(tsMillis >> 8)
	b[5] = byte(tsMillis)
	// crypto/rand.Read never returns an error as of Go 1.24.
	_, _ = rand.Read(b[6:])
	return crock.EncodeToString(b[:])
}
