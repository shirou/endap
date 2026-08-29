package record

import (
	"bytes"
	"encoding/json"
)

// wire is the shape encoding/json sees. Field order here is the field order in
// the emitted JSON, and matches the record layout in the spec.
type wire struct {
	V    int    `json:"v"`
	ID   string `json:"id"`
	TS   int64  `json:"ts"`
	Dur  *int64 `json:"dur,omitempty"`
	Cmd  string `json:"cmd"`
	Cwd  string `json:"cwd,omitempty"`
	Host string `json:"host"`
	Sess string `json:"sess,omitempty"`
	Exit *int   `json:"exit,omitempty"`
	Sh   string `json:"sh,omitempty"`
}

func (r *Record) wire() wire {
	w := wire{
		V:    r.V,
		ID:   r.ID,
		TS:   r.TS,
		Cmd:  r.Cmd,
		Cwd:  r.Cwd,
		Host: r.Host,
		Sess: r.Sess,
		Sh:   r.Sh,
	}
	if r.HasDur {
		d := r.Dur
		w.Dur = &d
	}
	if r.HasExit {
		e := r.Exit
		w.Exit = &e
	}
	return w
}

// Encode returns the JSONL line for r, including the trailing newline.
//
// The writer side deliberately stays on encoding/json: the hand-rolled decoder
// assumes the writer escapes correctly, so that correctness is delegated to the
// standard library (D4). SetEscapeHTML(false) is mandatory -- the default
// escapes < > &, which appear in most shell history lines (redirects, &&).
func Encode(r *Record) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	w := r.wire()
	if err := enc.Encode(&w); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
