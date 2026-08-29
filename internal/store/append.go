package store

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Append writes one already-encoded record line to the history log.
//
// The file is opened O_APPEND|O_WRONLY|O_CREAT and the line goes out in a
// single write(2), with no buffering and no lock: on Linux a buffered write to
// a regular file holds the inode lock, and O_APPEND makes the offset fetch and
// the write atomic together, so concurrent shells cannot interleave records.
//
// fsync is deliberately not called. A hook must not wait on the disk, and the
// cost of a crash is the last few records.
func Append(path string, line []byte) error {
	if err := EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, FilePerm)
	if err != nil {
		return err
	}
	err = writeWhole(f, line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// writeWhole writes buf in one write(2) and reports a short write as an error.
//
// A short write -- ENOSPC is the realistic cause -- stops in the middle of a
// record. Left alone, the fragment has no newline and the next append is
// concatenated onto it, so two records are lost inside a single corrupt line
// and one of them silently. Terminating the fragment costs one byte and turns
// that into exactly one corrupt line that `endap doctor` can count.
//
// The terminator is a second write, and nothing here holds a lock, so it is not
// race-free: if another shell appends between the truncated write and the
// newline, its record lands between the two and is swallowed by the same
// corrupt line. Closing that would mean locking every append, which is the cost
// the design refuses to pay on every prompt. What this does buy is the single
// writer case, which is the usual one, and a bound on the damage rather than an
// unterminated fragment that grows with each later append.
//
// It takes an io.Writer rather than the file so a test can inject the short
// write; a real *os.File cannot be made to do it on demand.
func writeWhole(w io.Writer, buf []byte) error {
	n, err := w.Write(buf)
	if err == nil && n < len(buf) {
		err = fmt.Errorf("short write: %d of %d bytes", n, len(buf))
	}
	if err != nil && n > 0 && n < len(buf) && buf[n-1] != '\n' {
		if _, werr := w.Write([]byte{'\n'}); werr != nil {
			err = fmt.Errorf("%w (and failed to terminate the fragment: %v)", err, werr)
		}
	}
	return err
}

// appendChunk is the largest single write a batch append makes. Each chunk
// holds whole records, so a concurrent shell appending its own record cannot
// end up inside one.
const appendChunk = 512 << 10

// AppendBatch writes many records with as few write(2) calls as it can while
// keeping every call atomic with respect to other appenders.
//
// Bulk paths like import must not buffer across record boundaries: a shell
// appending at the same moment would land in the middle of a half-written
// record.
func AppendBatch(path string, lines [][]byte) error {
	if len(lines) == 0 {
		return nil
	}
	if err := EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, FilePerm)
	if err != nil {
		return err
	}
	defer f.Close()
	return appendBatchTo(f, lines)
}

// appendBatchTo is AppendBatch once the file is open. It takes an io.Writer so
// a test can watch the chunk boundaries, which is the whole point of the
// batching: every write must end on a record boundary.
func appendBatchTo(f io.Writer, lines [][]byte) error {
	buf := make([]byte, 0, appendChunk+4096)
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		err := writeWhole(f, buf)
		buf = buf[:0]
		return err
	}
	for _, l := range lines {
		buf = append(buf, l...)
		if len(l) == 0 || l[len(l)-1] != '\n' {
			buf = append(buf, '\n')
		}
		if len(buf) >= appendChunk {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}
