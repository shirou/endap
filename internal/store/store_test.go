package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/shirou/endap/internal/record"
)

func line(t *testing.T, r record.Record) []byte {
	t.Helper()
	r.V = record.Version
	if r.ID == "" {
		r.ID = record.NewID(r.TS)
	}
	b, err := record.Encode(&r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSplitLines(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a\n", []string{"a"}},
		{"a\nb\n", []string{"a", "b"}},
		{"a\nb", []string{"a", "b"}},
		{"\n", []string{""}},
		// A partial write leaves a fragment with no newline. It has to survive
		// as its own line so it is counted as exactly one corrupt line.
		{"{\"v\":1}\n{\"v\":1", []string{`{"v":1}`, `{"v":1`}},
	}
	for _, c := range cases {
		var got []string
		for _, l := range SplitLines([]byte(c.in)) {
			got = append(got, string(l))
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("SplitLines(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		line string
		want Kind
	}{
		{`{"v":1,"id":"A","ts":1,"cmd":"ls","host":"h"}`, KindRecord},
		{`{"v":99,"id":"A","ts":1,"cmd":"ls","host":"h"}`, KindUnknown},
		{`{"v":0,"id":"A","ts":1,"cmd":"ls","host":"h"}`, KindCorrupt},
		// Valid JSON without a v is not an endap record.
		{`{"id":"A","ts":1,"cmd":"ls","host":"h"}`, KindCorrupt},
		{`{"v":-1,"cmd":"ls"}`, KindCorrupt},
		{`not json`, KindCorrupt},
		{``, KindCorrupt},
	}
	for _, c := range cases {
		var r record.Record
		if got := Classify([]byte(c.line), &r); got != c.want {
			t.Errorf("Classify(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

// TestConcurrentAppend is V2. Records must never interleave.
//
// The sizes matter more than the process count. At a few hundred bytes a
// record fits in a default bufio.Writer, so even a wrong implementation that
// buffers and flushes once per record produces a valid file. Only records
// larger than the buffer, and larger than a pipe's worth of bytes, actually
// exercise the single-write(2) guarantee.
func TestConcurrentAppend(t *testing.T) {
	// The plan asks for 32 writers x 100 records. That scale is affordable at
	// the ordinary record size; at 1MiB it would mean writing 3.2GB to check a
	// property the smaller runs already exercise, so the large sizes trade
	// count for size. Goroutines rather than processes: each Append opens its
	// own descriptor with O_APPEND, which is what the atomicity claim rests on.
	for _, c := range []struct {
		size, writers, perWriter int
	}{
		{200, 32, 100},
		{64 << 10, 16, 20},
		{1 << 20, 8, 5},
	} {
		t.Run(fmt.Sprintf("size=%d", c.size), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, HistoryName)
			writers, perWriter := c.writers, c.perWriter
			payload := strings.Repeat("x", c.size)
			var wg sync.WaitGroup
			for w := range writers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range perWriter {
						rec := record.Record{
							V: record.Version, ID: record.NewID(int64(i)), TS: int64(i),
							Cmd: fmt.Sprintf("w%d-i%d-%s", w, i, payload), Host: "h",
						}
						b, err := record.Encode(&rec)
						if err != nil {
							t.Error(err)
							return
						}
						if err := Append(path, b); err != nil {
							t.Error(err)
							return
						}
					}
				}()
			}
			wg.Wait()

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := SplitLines(data)
			if len(lines) != writers*perWriter {
				t.Fatalf("got %d lines, want %d", len(lines), writers*perWriter)
			}
			seen := map[string]bool{}
			for _, l := range lines {
				var r record.Record
				if err := record.Decode(l, &r); err != nil {
					t.Fatalf("line does not parse (%d bytes): %v", len(l), err)
				}
				if !json.Valid(l) {
					t.Fatalf("line is not valid JSON")
				}
				if seen[r.Cmd] {
					t.Fatalf("duplicate record %.30q", r.Cmd)
				}
				seen[r.Cmd] = true
			}
		})
	}
}

// shortWriter accepts limit bytes on its first write and then behaves the way
// its mode says. A real *os.File cannot be asked to write short on demand, so
// this is how the D11 isolation path becomes reachable.
type shortWriter struct {
	buf    bytes.Buffer
	limit  int
	err    error // what the truncated write reports, nil for a bare short count
	refuse bool  // whether later writes fail too, as a full disk would
	done   bool
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if w.done {
		if w.refuse {
			return 0, syscall.ENOSPC
		}
		w.buf.Write(p)
		return len(p), nil
	}
	w.done = true
	n := min(len(p), w.limit)
	w.buf.Write(p[:n])
	if n < len(p) {
		return n, w.err
	}
	return n, nil
}

// TestWriteWholeIsolatesAPartialWrite is the D11 ENOSPC case. The fragment left
// by a short write has to be terminated, or the next append is concatenated
// onto it and two records are lost inside one corrupt line -- one of them
// without any sign that it ever existed.
func TestWriteWholeIsolatesAPartialWrite(t *testing.T) {
	rec := line(t, record.Record{TS: 1, Cmd: "a command long enough to be cut in half", Host: "h"})
	for _, c := range []struct {
		name string
		err  error
	}{
		// A Writer that reports the truncation as an error, and one that just
		// returns a short count. Both are short writes as far as endap is
		// concerned, and os.File.Write does not retry either.
		{"reported as an error", syscall.ENOSPC},
		{"reported as a short count", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := &shortWriter{limit: len(rec) / 2, err: c.err}
			err := writeWhole(w, rec)
			if err == nil {
				t.Fatal("a short write was reported as success")
			}
			got := w.buf.Bytes()
			if len(got) == 0 || got[len(got)-1] != '\n' {
				t.Fatalf("the fragment was not terminated: %q", got)
			}
			if bytes.Count(got, []byte{'\n'}) != 1 {
				t.Fatalf("expected exactly one line: %q", got)
			}
			// Read back what the log would then hold: the fragment as one
			// corrupt line, and the next good record intact rather than glued
			// onto it.
			next := line(t, record.Record{TS: 2, Cmd: "ls", Host: "h"})
			st := Walk(append(append([]byte(nil), got...), next...), nil)
			if st.Corrupt != 1 || st.Records != 1 {
				t.Fatalf("got %+v, want exactly 1 corrupt line and 1 record", st)
			}
		})
	}
}

// TestWriteWholeReportsAFullDisk: when even the terminating newline will not
// fit, the caller has to hear about both failures rather than the second one
// hiding the first.
func TestWriteWholeReportsAFullDisk(t *testing.T) {
	rec := line(t, record.Record{TS: 1, Cmd: "a command long enough to be cut in half", Host: "h"})
	w := &shortWriter{limit: len(rec) / 2, err: syscall.ENOSPC, refuse: true}
	err := writeWhole(w, rec)
	if err == nil {
		t.Fatal("a short write on a full disk was reported as success")
	}
	if !strings.Contains(err.Error(), "failed to terminate the fragment") {
		t.Fatalf("the second failure was swallowed: %v", err)
	}
}

// TestWriteWholeLeavesAWholeWriteAlone guards the other direction: no stray
// newline on the ordinary path.
func TestWriteWholeLeavesAWholeWriteAlone(t *testing.T) {
	rec := line(t, record.Record{TS: 1, Cmd: "ls", Host: "h"})
	w := &shortWriter{limit: len(rec)}
	if err := writeWhole(w, rec); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.buf.Bytes(), rec) {
		t.Fatalf("got %q, want %q", w.buf.Bytes(), rec)
	}
}

func TestRewriteKeepsUnknownVersionsAndParksCorruptLines(t *testing.T) {
	// V9: a record from a newer endap has to survive a rewrite untouched, and a
	// duplicate of it has to be recognised without being parsed.
	dir := t.TempDir()
	path := filepath.Join(dir, HistoryName)
	future := []byte(`{"v":99,"id":"FUT","ts":5,"cmd":"tomorrow","host":"h","extra":{"a":[1]}}` + "\n")
	var buf bytes.Buffer
	buf.Write(line(t, record.Record{TS: 1, Cmd: "ls", Host: "h"}))
	buf.Write(future)
	buf.WriteString("this line is not json\n")
	buf.Write(future)
	if err := os.WriteFile(path, buf.Bytes(), FilePerm); err != nil {
		t.Fatal(err)
	}

	seenRaw := map[string]bool{}
	var rej [][]byte
	err := Rewrite(path, func(lines [][]byte) ([][]byte, error) {
		var out [][]byte
		var r record.Record
		for _, l := range lines {
			switch Classify(l, &r) {
			case KindCorrupt:
				rej = append(rej, append([]byte(nil), l...))
			case KindUnknown:
				if seenRaw[string(l)] {
					continue
				}
				seenRaw[string(l)] = true
				out = append(out, l)
			default:
				out = append(out, l)
			}
		}
		return out, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendRej(path, rej); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte(`"v":99`)) {
		t.Fatalf("the unknown-version record was dropped:\n%s", data)
	}
	if n := bytes.Count(data, []byte(`"v":99`)); n != 1 {
		t.Fatalf("the unknown-version record appears %d times, want 1", n)
	}
	if bytes.Contains(data, []byte("not json")) {
		t.Fatalf("the corrupt line stayed in the log")
	}
	rejData, err := os.ReadFile(RejPath(path))
	if err != nil || !bytes.Contains(rejData, []byte("not json")) {
		t.Fatalf("the corrupt line did not reach the .rej file: %v %s", err, rejData)
	}
	// The rewritten log keeps 0600.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != FilePerm {
		t.Fatalf("rewritten log is %#o, want %#o", info.Mode().Perm(), FilePerm)
	}
}

// TestRewriteCatchesUpWithAppends is V8, step 3: a record appended after the
// head was measured has to be copied into the temporary before the rename.
//
// The assertion is made inside beforeRename, on the temporary itself. Checking
// the finished log instead would pass even with step 3 deleted, because step 5
// would recover the same record after the rename and leave the file looking
// identical.
func TestRewriteCatchesUpWithAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, HistoryName)
	if err := Append(path, line(t, record.Record{TS: 1, Cmd: "original", Host: "h"})); err != nil {
		t.Fatal(err)
	}
	duringBuild := line(t, record.Record{TS: 2, Cmd: "appended during build", Host: "h"})

	caughtUp := false
	beforeRename = func(tmpPath string) {
		tmp, err := os.ReadFile(tmpPath)
		if err != nil {
			t.Error(err)
			return
		}
		caughtUp = bytes.Contains(tmp, []byte("appended during build"))
	}
	t.Cleanup(func() { beforeRename = nil })

	err := Rewrite(path, func(lines [][]byte) ([][]byte, error) {
		// This lands after S0 and has to be picked up by the catch-up read.
		if err := Append(path, duringBuild); err != nil {
			return nil, err
		}
		return lines, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !caughtUp {
		t.Fatal("the catch-up read did not copy the record into the temporary before the rename")
	}
	data, _ := os.ReadFile(path)
	if n := bytes.Count(data, []byte("appended during build")); n != 1 {
		t.Fatalf("the record appears %d times in the log, want 1:\n%s", n, data)
	}
}

// TestRewriteRecoversAfterRename drives D7 step 5: a record written to the old
// inode after the last catch-up round but before the rename.
//
// It needs the beforeRename seam. Appending from inside BuildFunc lands before
// the catch-up rounds, which pick it up at step 3 and leave the recovery code
// unexecuted -- so without the seam this path has no test at all, and deleting
// it would break nothing that is checked.
func TestRewriteRecoversAfterRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, HistoryName)
	if err := Append(path, line(t, record.Record{TS: 1, Cmd: "original", Host: "h"})); err != nil {
		t.Fatal(err)
	}
	late := line(t, record.Record{TS: 3, Cmd: "written just before the rename", Host: "h"})

	beforeRename = func(string) {
		// A shell that opened the log before the rename writes here. The
		// descriptor still points at the inode the rename is about to unlink.
		if err := Append(path, late); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeRename = nil })

	if err := Rewrite(path, func(lines [][]byte) ([][]byte, error) { return lines, nil }); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte("written just before the rename")) {
		t.Fatalf("step 5 did not recover the record appended before the rename:\n%s", data)
	}
	if n := bytes.Count(data, []byte("written just before the rename")); n != 1 {
		t.Fatalf("the recovered record appears %d times, want 1", n)
	}
	if !bytes.Contains(data, []byte("original")) {
		t.Fatalf("the rewrite lost the original record:\n%s", data)
	}
}

func TestRewriteRefusesWhenLocked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, HistoryName)
	if err := Append(path, line(t, record.Record{TS: 1, Cmd: "ls", Host: "h"})); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(LockPath(path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, FilePerm)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	err = Rewrite(path, func(lines [][]byte) ([][]byte, error) { return nil, nil })
	if err == nil {
		t.Fatal("Rewrite succeeded while the lock was held")
	}
	if !strings.Contains(err.Error(), "another rewrite is in progress") {
		t.Fatalf("unexpected error: %v", err)
	}
	// The log must be untouched.
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte("ls")) {
		t.Fatalf("the log was modified despite the lock: %s", data)
	}
}

func TestRewriteReleasesTheLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, HistoryName)
	if err := Append(path, line(t, record.Record{TS: 1, Cmd: "ls", Host: "h"})); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Rewrite(path, func(l [][]byte) ([][]byte, error) { return l, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(LockPath(path)); !os.IsNotExist(err) {
		t.Fatalf("the lock file outlived the rewrite")
	}
	if tmps, _ := TmpFiles(path); len(tmps) != 0 {
		t.Fatalf("temporary files were left behind: %v", tmps)
	}
}

func TestTmpFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, HistoryName)
	os.WriteFile(path, nil, FilePerm)
	os.WriteFile(path+".tmp.1234", nil, FilePerm)
	os.WriteFile(RejPath(path), nil, FilePerm)
	got, err := TmpFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || filepath.Base(got[0]) != HistoryName+".tmp.1234" {
		t.Fatalf("TmpFiles = %v", got)
	}
}

// TestConcurrentCompactIsRefused is the second half of V8: two rewrites at once
// must not both proceed, because they share a fixed temporary name space.
func TestConcurrentCompactIsRefused(t *testing.T) {
	bin := buildEndap(t)
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, DirPerm); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, HistoryName)
	for i := range 200 {
		if err := Append(path, line(t, record.Record{TS: int64(i), Cmd: fmt.Sprintf("cmd %d", i), Host: "h"})); err != nil {
			t.Fatal(err)
		}
	}
	// Hold the lock so the second process is guaranteed to see it.
	f, err := os.OpenFile(LockPath(path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, FilePerm)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	cmd := exec.Command(bin, "compact", "--yes")
	cmd.Env = append(os.Environ(), "ENDAP_DATA_DIR="+dataDir, "ENDAP_CONFIG="+filepath.Join(dir, "no-config"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("compact succeeded while locked: %s", out)
	}
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("compact exited %d, want 1: %s", code, out)
	}
	if !strings.Contains(string(out), "another rewrite is in progress") {
		t.Fatalf("unexpected output: %s", out)
	}
}

// buildEndap compiles the command under test into the test's temp directory.
func buildEndap(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	bin := filepath.Join(t.TempDir(), "endap")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/shirou/endap")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building endap: %v\n%s", err, out)
	}
	return bin
}

// TestAppendBatch covers the bulk path import uses. It must not buffer across a
// record boundary: a shell appending at the same moment would otherwise land in
// the middle of a half-written record.
func TestAppendBatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, HistoryName)
	var lines [][]byte
	for i := range 500 {
		lines = append(lines, line(t, record.Record{TS: int64(i), Cmd: fmt.Sprintf("cmd %d", i), Host: "h"}))
	}
	// One record larger than the chunk size, so the chunking is actually used.
	lines = append(lines, line(t, record.Record{TS: 999, Cmd: strings.Repeat("x", 700<<10), Host: "h"}))
	if err := AppendBatch(path, lines); err != nil {
		t.Fatal(err)
	}
	st, err := WalkFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Records != len(lines) || st.Corrupt != 0 {
		t.Fatalf("got %+v, want %d records and no corruption", st, len(lines))
	}
	// A second batch appends rather than replacing.
	if err := AppendBatch(path, lines[:1]); err != nil {
		t.Fatal(err)
	}
	st, _ = WalkFile(path, nil)
	if st.Records != len(lines)+1 {
		t.Fatalf("got %d records, want %d", st.Records, len(lines)+1)
	}
	if err := AppendBatch(path, nil); err != nil {
		t.Fatalf("an empty batch should be a no-op: %v", err)
	}
}

// chunkRecorder remembers the size and last byte of every write it is given.
type chunkRecorder struct {
	sizes []int
	ends  []byte
}

func (c *chunkRecorder) Write(p []byte) (int, error) {
	c.sizes = append(c.sizes, len(p))
	if len(p) > 0 {
		c.ends = append(c.ends, p[len(p)-1])
	}
	return len(p), nil
}

// TestAppendBatchChunksOnRecordBoundaries is the reason AppendBatch exists
// rather than a bufio.Writer. Each write(2) is atomic against other appenders
// only if it contains whole records, so a chunk must never stop mid-record.
func TestAppendBatchChunksOnRecordBoundaries(t *testing.T) {
	var lines [][]byte
	// Enough ordinary records to cross the chunk size several times, plus one
	// record larger than a chunk on its own.
	for i := range 20000 {
		lines = append(lines, line(t, record.Record{TS: int64(i), Cmd: fmt.Sprintf("cmd %d", i), Host: "h"}))
	}
	lines = append(lines, line(t, record.Record{TS: 1, Cmd: strings.Repeat("x", 2*appendChunk), Host: "h"}))
	for i := range 2000 {
		lines = append(lines, line(t, record.Record{TS: int64(i), Cmd: "tail", Host: "h"}))
	}

	rec := &chunkRecorder{}
	if err := appendBatchTo(rec, lines); err != nil {
		t.Fatal(err)
	}
	if len(rec.sizes) < 3 {
		t.Fatalf("everything went out in %d writes; the batching is not splitting at all", len(rec.sizes))
	}
	for i, end := range rec.ends {
		if end != '\n' {
			t.Fatalf("write %d of %d ends mid-record (last byte %q), so a concurrent append could land inside it",
				i, len(rec.sizes), end)
		}
	}
	// A chunk may overshoot by at most the record that crossed the threshold.
	for i, n := range rec.sizes {
		if n > appendChunk && n < 2*appendChunk {
			continue // the oversized record itself
		}
		if n > 3*appendChunk {
			t.Fatalf("write %d is %d bytes, far past the %d chunk size", i, n, appendChunk)
		}
	}
}

func TestDataDirPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := []struct {
		name  string
		endap string
		xdg   string
		want  string
	}{
		{"ENDAP_DATA_DIR wins", "/explicit", "/xdg", "/explicit"},
		{"XDG_DATA_HOME next", "", "/xdg", "/xdg/endap"},
		{"home last", "", "", filepath.Join(home, ".local", "share", "endap")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ENDAP_DATA_DIR", c.endap)
			t.Setenv("XDG_DATA_HOME", c.xdg)
			got, err := DataDir()
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("DataDir() = %q, want %q", got, c.want)
			}
			hist, err := HistoryPath()
			if err != nil {
				t.Fatal(err)
			}
			if hist != filepath.Join(c.want, HistoryName) {
				t.Fatalf("HistoryPath() = %q", hist)
			}
		})
	}
}

func TestConfigPathPrecedence(t *testing.T) {
	t.Setenv("ENDAP_CONFIG", "/explicit/config")
	got, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/explicit/config" {
		t.Fatalf("ConfigPath() = %q", got)
	}
	t.Setenv("ENDAP_CONFIG", "")
	t.Setenv("ENDAP_CONFIG_DIR", "/dir")
	got, err = ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join("/dir", "config") {
		t.Fatalf("ConfigPath() = %q", got)
	}
}

func TestPathHelpersShareOneSpelling(t *testing.T) {
	const h = "/data/history.jsonl"
	if got := RejPath(h); got != h+RejSuffix {
		t.Errorf("RejPath = %q", got)
	}
	if got := LockPath(h); got != h+LockSuffix {
		t.Errorf("LockPath = %q", got)
	}
	if RejName != HistoryName+RejSuffix {
		t.Errorf("RejName = %q", RejName)
	}
}
