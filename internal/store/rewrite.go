package store

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrLocked means another rewrite holds the lock.
var ErrLocked = errors.New("another rewrite is in progress")

// catchUpRounds bounds how many times a rewrite re-reads the tail. A log that
// keeps growing faster than it can be copied is not going to settle, so the
// rewrite stops chasing it and goes ahead.
const catchUpRounds = 3

// beforeRename runs just before the rename, with the path of the temporary, and
// is nil outside tests.
//
// Steps 3 and 5 are indistinguishable from outside: whichever one picks a record
// up, it ends up in the same place in the same file. Without a way to look at
// the temporary before the rename and to write after the last catch-up round,
// deleting either step breaks nothing that any test checks -- which for two
// data-loss paths is the same as not having written them.
var beforeRename func(tmpPath string)

// BuildFunc turns the lines of the existing log into the lines of the new one.
// The input lines are sub-slices of one buffer and carry no trailing newline;
// the returned lines are written with a newline each.
//
// Rewrite calls it exactly once. Callers that accumulate state across the call
// -- merge, which folds in records read before the lock was taken -- depend on
// that, so it is part of the contract rather than an accident of the code.
type BuildFunc func(lines [][]byte) ([][]byte, error)

// Rewrite replaces the history log with the output of build.
//
// This is the one place compact, forget and merge --in-place all go through,
// and it is shaped by a race the plain "read everything, write a temp file,
// rename" recipe loses: a shell appending a record while the rewrite is in
// flight would have it dropped by the rename.
//
// The sequence, folding D7's steps 3 and 4 into one, is:
//
//  0. take an exclusive lock, so two rewrites cannot destroy each other's temp
//     file. Nothing on the append or read path ever looks at this lock.
//  1. open the log and take its size from the same descriptor. Calling os.Stat
//     first and opening afterwards would let an append land in between and be
//     read twice.
//  2. transform only the bytes in [0, S0).
//  3. append whatever arrived after S0 verbatim, repeating while the file grows.
//  4. fsync and rename.
//  5. read the old inode once more through the descriptor still held open, and
//     append anything that arrived between the last check and the rename.
//
// The window does not close completely, in two ways.
//
// A write that is in flight at the moment of the rename lands in an inode that
// is already unlinked, and no amount of re-reading recovers it. Step 5 shrinks
// that window to microseconds; closing it would require the append path to take
// a lock, which is exactly the cost the design refuses to pay on every prompt.
//
// And step 5 only runs if this process lives to run it. Killed between the
// rename and the recovery, the records appended after the last catch-up round
// exist solely in an inode that now has no name, and closing the descriptor
// frees them. Surviving that would need the old file kept under a recovery name
// until the recovery completed -- a persistent protocol for a window measured in
// microseconds, on an operation the user runs by hand.
func Rewrite(history string, build BuildFunc) error {
	lock, err := acquireLock(history)
	if err != nil {
		return err
	}
	defer os.Remove(lock)

	f, err := os.Open(history)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	s0, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}

	head := make([]byte, s0)
	if _, err := io.ReadFull(io.NewSectionReader(f, 0, s0), head); err != nil {
		return fmt.Errorf("read %s: %w", history, err)
	}
	out, err := build(SplitLines(head))
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(history), filepath.Base(history)+TmpSuffix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()
	if err := tmp.Chmod(FilePerm); err != nil {
		return err
	}
	// Buffered: this temp file is private to the rewrite until the rename, so
	// there is no other writer to interleave with, and 100k lines is 200k write
	// syscalls without it.
	w := bufio.NewWriterSize(tmp, 256<<10)
	if err := writeLines(w, out); err != nil {
		return err
	}

	// Catch up with records appended while the head was being transformed.
	off := s0
	for range catchUpRounds {
		end, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			return err
		}
		if end <= off {
			break
		}
		if _, err := io.Copy(w, io.NewSectionReader(f, off, end-off)); err != nil {
			return err
		}
		off = end
	}

	if err := w.Flush(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if beforeRename != nil {
		beforeRename(tmpName)
	}
	if err := os.Rename(tmpName, history); err != nil {
		return err
	}

	// The old inode is still reachable through f. Anything appended between the
	// last catch-up round and the rename is only there.
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if end > off {
		tail := make([]byte, end-off)
		if _, err := io.ReadFull(io.NewSectionReader(f, off, end-off), tail); err != nil {
			return err
		}
		if err := Append(history, tail); err != nil {
			return fmt.Errorf("recover records appended during the rewrite: %w", err)
		}
	}
	return nil
}

// acquireLock creates the lock file exclusively.
//
// This locks rewrites against each other, not against appends: `endap add` and
// `endap list` never look at it, so the prompt is never blocked by a compaction.
func acquireLock(history string) (string, error) {
	path := LockPath(history)
	if err := EnsureDir(filepath.Dir(path)); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, FilePerm)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("%w (%s); remove it if nothing is running", ErrLocked, path)
		}
		return "", err
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	f.Close()
	return path, nil
}

// ReplaceFile atomically rewrites path with lines, or removes it when there are
// none.
//
// It is the same temp-file-and-rename dance Rewrite performs, without the lock
// and the catch-up rounds, for the files nothing else appends to: the
// rejected-lines file and the temporaries a crashed rewrite left behind. Those
// hold the same records as the log, so a secret is not gone until they are
// rewritten too, and they have to end up with the same 0600 as everything else.
func ReplaceFile(path string, lines [][]byte) error {
	if len(lines) == 0 {
		return os.Remove(path)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".new.*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(name)
	}()
	if err := tmp.Chmod(FilePerm); err != nil {
		return err
	}
	w := bufio.NewWriterSize(tmp, 64<<10)
	if err := writeLines(w, lines); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// writeLines writes each line followed by a newline.
//
// The newline is a separate call on purpose: the lines are sub-slices of one
// buffer, so appending to them would write over the start of the next one.
func writeLines(w io.Writer, lines [][]byte) error {
	for _, l := range lines {
		if _, err := w.Write(l); err != nil {
			return err
		}
		if _, err := w.Write([]byte{'\n'}); err != nil {
			return err
		}
	}
	return nil
}

// AppendRej parks lines that could not be parsed.
//
// Corrupt lines are never simply dropped: they may hold a command that was
// written during a partial write, and the point of the rewrite is to tidy the
// log, not to lose data nobody has looked at yet.
func AppendRej(history string, lines [][]byte) error {
	if len(lines) == 0 {
		return nil
	}
	var buf []byte
	for _, l := range lines {
		buf = append(buf, l...)
		buf = append(buf, '\n')
	}
	return Append(RejPath(history), buf)
}
