package record

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

// decodeStd is the reference implementation of Decode, built on encoding/json.
//
// It lives in a test file on purpose: keeping it in the product would leave a
// permanently unused function behind, and its only job is to be the other side
// of the differential test (V4) that guards the hand-rolled parser.
func decodeStd(line []byte, dst *Record) error {
	*dst = Record{}
	var m map[string]json.RawMessage
	rd := bytes.NewReader(line)
	dec := json.NewDecoder(rd)
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	// Unmarshalling a bare "null" into a map succeeds and leaves it nil, so the
	// map target alone does not establish that the line was an object. A record
	// has to be one.
	if m == nil {
		return fmt.Errorf("%w: not an object", ErrMalformed)
	}
	// Reject trailing content so both decoders agree on "one line, one record".
	// Only the four characters JSON calls whitespace are allowed to follow;
	// bytes.TrimSpace would also swallow \f and \v, which JSON does not permit
	// and which the hand-rolled parser correctly rejects.
	rest, _ := io.ReadAll(io.MultiReader(dec.Buffered(), rd))
	if len(bytes.Trim(rest, " \t\n\r")) != 0 {
		return fmt.Errorf("%w: trailing bytes", ErrMalformed)
	}
	// Keys are matched exactly. Unmarshalling straight into the wire struct
	// would also accept "Cmd" and "CMD", which is a struct-tag convenience the
	// on-disk format should not inherit.
	str := func(key string, dst *string) error {
		raw, ok := m[key]
		if !ok || string(raw) == "null" {
			return nil
		}
		return json.Unmarshal(raw, dst)
	}
	num := func(key string) (int64, bool, error) {
		raw, ok := m[key]
		if !ok || string(raw) == "null" {
			return 0, false, nil
		}
		var v int64
		if err := json.Unmarshal(raw, &v); err != nil {
			return 0, false, err
		}
		return v, true, nil
	}
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"id", &dst.ID}, {"cmd", &dst.Cmd}, {"cwd", &dst.Cwd},
		{"host", &dst.Host}, {"sess", &dst.Sess}, {"sh", &dst.Sh},
	} {
		if err := str(f.key, f.dst); err != nil {
			return fmt.Errorf("%w: %v", ErrMalformed, err)
		}
	}
	for _, f := range []struct {
		key string
		set func(int64)
	}{
		{"v", func(n int64) { dst.V = int(n) }},
		{"ts", func(n int64) { dst.TS = n }},
		{"dur", func(n int64) { dst.SetDur(n) }},
		{"exit", func(n int64) { dst.SetExit(int(n)) }},
	} {
		n, ok, err := num(f.key)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		if ok {
			f.set(n)
		}
	}
	return nil
}

func TestDecodeMatchesStdlib(t *testing.T) {
	lines := []string{
		`{"v":1,"id":"A","ts":1,"cmd":"ls","host":"h"}`,
		`{"v":1,"id":"A","ts":1,"dur":0,"cmd":"ls","cwd":"/tmp","host":"h","sess":"s","exit":0,"sh":"zsh"}`,
		`{"v":1,"id":"A","ts":1,"dur":842,"cmd":"grep foo > out.log 2>&1 && echo done","host":"h","exit":1}`,
		`{"v":1,"id":"A","ts":1,"cmd":"echo \\","host":"h"}`,
		`{"v":1,"id":"A","ts":1,"cmd":"echo \\\\","host":"h"}`,
		`{"v":1,"id":"A","ts":1,"cmd":"a\nb\tc\"d\\e/f","host":"h"}`,
		`{"v":1,"id":"A","ts":1,"cmd":"\u0000\u001f\u2028\u2029","host":"h"}`,
		`{"v":1,"id":"A","ts":1,"cmd":"\ud83d\ude00","host":"h"}`,
		`{"v":1,"id":"A","ts":1,"cmd":"\ud800 lone","host":"h"}`,
		`{"v":1,"id":"A","ts":1,"cmd":"\"cmd\":\"decoy\"","host":"h"}`,
		`{"v":99,"id":"A","ts":1,"cmd":"future","host":"h","newfield":{"a":[1,2,{"b":null}]}}`,
		`{"v":1,"id":"A","ts":-5,"cmd":"neg","host":"h","exit":-1}`,
		`{"v":1,"id":null,"ts":1,"cmd":"nulls","host":"h","dur":null,"exit":null}`,
		` {"v":1,"id":"A","ts":1,"cmd":"ws","host":"h"} `,
		`{}`,
		// A key may repeat, and the last occurrence wins, so a wrong-typed
		// earlier value is not on its own a reason to reject the record.
		`{"cmd":0,"cmd":"ls"}`,
		`{"ts":1.5,"ts":2}`,
		`{"dur":"x","dur":7}`,
		// A trailing null is the last value, so it has to clear the earlier one.
		`{"v":1,"dur":42,"dur":null}`,
		`{"v":1,"exit":1,"exit":null}`,
		`{"v":1,"cmd":"ls","cmd":null}`,
		`{"v":1,"ts":5,"ts":null}`,
		`{"v":1,"v":null}`,
		`{"v":1,"sess":"a","sess":null,"sess":"b"}`,
		// ... but a wrong-typed last occurrence is.
		`{"cmd":"ls","cmd":0}`,
		`{"cmd":0}`,
		`{"ts":"1"}`,
		// \f and \v are whitespace to Unicode but not to JSON.
		"{}\f",
		"{}\v",
		"{} ",
		"{}\n",
		// malformed
		``,
		`not json`,
		`{"v":1,`,
		`{"cmd":"unterminated}`,
		`{"v":1,"id":"A","ts":1,"cmd":"x","host":"h"} {"v":1}`,
		`[1,2,3]`,
		// A bare null is not a record, however willing encoding/json is to
		// unmarshal it into a map.
		`null`,
		`true`,
		`"a string"`,
		`{"cmd":"bad\qescape"}`,
	}
	for _, line := range lines {
		var got, want Record
		gotErr := Decode([]byte(line), &got)
		wantErr := decodeStd([]byte(line), &want)
		if (gotErr == nil) != (wantErr == nil) {
			t.Errorf("line %q: decode err=%v, std err=%v", line, gotErr, wantErr)
			continue
		}
		if gotErr != nil {
			continue
		}
		if got != want {
			t.Errorf("line %q:\n got %+v\nwant %+v", line, got, want)
		}
	}
}

// TestDecodeNestingLimit pins the limit to encoding/json's. It has to be the
// same number in both, or the differential test turns a depth difference into a
// fuzz failure; it also keeps a pathological line from exhausting the stack.
func TestDecodeNestingLimit(t *testing.T) {
	nest := func(depth int) []byte {
		return []byte(`{"v":1,"id":"A","ts":1,"cmd":"x","host":"h","z":` +
			strings.Repeat("[", depth) + strings.Repeat("]", depth) + `}`)
	}
	// The record object is the first container, so depth-1 arrays fit exactly.
	var r Record
	if err := Decode(nest(maxDepth-1), &r); err != nil {
		t.Fatalf("depth %d should be accepted: %v", maxDepth-1, err)
	}
	if err := Decode(nest(maxDepth), &r); err == nil {
		t.Fatalf("depth %d should be rejected", maxDepth)
	}
	// The reference implementation has to agree at the boundary.
	for _, d := range []int{maxDepth - 1, maxDepth} {
		var got, want Record
		gotErr := Decode(nest(d), &got)
		wantErr := decodeStd(nest(d), &want)
		if (gotErr == nil) != (wantErr == nil) {
			t.Errorf("depth %d: decode err=%v, std err=%v", d, gotErr, wantErr)
		}
	}
	// A line deep enough to overflow the stack must return an error instead.
	if err := Decode(nest(1<<20), &r); err == nil {
		t.Fatal("a deeply nested line was accepted")
	}
}

func FuzzDecodeAgainstStdlib(f *testing.F) {
	f.Add(`{"v":1,"id":"A","ts":1,"cmd":"ls","host":"h"}`)
	f.Add(`{"v":1,"cmd":"echo \\","host":"h"}`)
	f.Add(`{"v":1,"cmd":"\u2028\u2029\ud83d\ude00","host":"h"}`)
	f.Add(`{"v":1,"cmd":"a > b && c","host":"h","x":[{"y":1}]}`)
	f.Add(`{`)
	f.Add(`{"cmd":0,"cmd":"ls"}`)
	f.Add(`{"v":1,"dur":42,"dur":null}`)
	f.Add(`null`)
	f.Add("{}\f")
	f.Fuzz(func(t *testing.T, line string) {
		var got, want Record
		gotErr := Decode([]byte(line), &got)
		wantErr := decodeStd([]byte(line), &want)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("disagree on %q: decode err=%v, std err=%v", line, gotErr, wantErr)
		}
		if gotErr == nil && got != want {
			t.Fatalf("disagree on %q:\n got %+v\nwant %+v", line, got, want)
		}
	})
}

// FuzzRoundTrip is V3: arbitrary command strings must survive encode -> decode.
func FuzzRoundTrip(f *testing.F) {
	seeds := []string{
		"", " ", "ls", "echo a | tr a b", "grep foo bar.txt > out.log 2>&1 && echo done",
		"echo \\", "echo \\\\", "echo \\\\\\", `"cmd":"decoy"`, "a\nb", "a\tb", "a\rb",
		"\u2028\u2029", "\x00\x01\x1f", "日本語", "\U0001F600", strings.Repeat("x", 5000),
		"multi\nline\ncommand\n", `{"v":1,"cmd":"nested"}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, cmd string) {
		r := Record{V: Version, ID: NewID(0), TS: 12345, Cmd: cmd, Host: "h"}
		r.SetExit(0)
		line, err := Encode(&r)
		if err != nil {
			t.Fatalf("encode %q: %v", cmd, err)
		}
		// A record is always exactly one physical line.
		if n := bytes.Count(line, []byte{'\n'}); n != 1 || line[len(line)-1] != '\n' {
			t.Fatalf("encode %q produced %d newlines: %q", cmd, n, line)
		}
		var got Record
		if err := Decode(line[:len(line)-1], &got); err != nil {
			t.Fatalf("decode %q (%q): %v", cmd, line, err)
		}
		if utf8.ValidString(cmd) {
			// Byte-for-byte round trip only holds for valid UTF-8: encoding/json
			// replaces invalid bytes with U+FFFD, so anything else is lossy by
			// construction (D4).
			if got.Cmd != cmd {
				t.Fatalf("round trip lost data:\n got %q\nwant %q", got.Cmd, cmd)
			}
		}
		if got.TS != 12345 || got.Host != "h" || !got.HasExit || got.Exit != 0 {
			t.Fatalf("round trip lost metadata: %+v", got)
		}
	})
}
