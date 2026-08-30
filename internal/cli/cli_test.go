package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shirou/endap/internal/record"
	"github.com/shirou/endap/internal/store"
)

type harness struct {
	t      *testing.T
	dir    string
	stdout bytes.Buffer
	stderr bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ENDAP_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("ENDAP_CONFIG", filepath.Join(dir, "config"))
	t.Setenv("ENDAP_HOST", "testhost")
	return &harness{t: t, dir: dir}
}

func (h *harness) env(stdin string) *Env {
	h.stdout.Reset()
	h.stderr.Reset()
	return &Env{Stdin: strings.NewReader(stdin), Stdout: &h.stdout, Stderr: &h.stderr}
}

func (h *harness) run(fn func(*Env, []string) int, args ...string) int {
	return fn(h.env(""), args)
}

func (h *harness) historyPath() string {
	return filepath.Join(h.dir, "data", store.HistoryName)
}

func (h *harness) log() string {
	b, err := os.ReadFile(h.historyPath())
	if err != nil {
		return ""
	}
	return string(b)
}

func (h *harness) records() []record.Record {
	var out []record.Record
	data, _ := os.ReadFile(h.historyPath())
	store.Walk(data, func(r *record.Record) { out = append(out, *r) })
	return out
}

func (h *harness) writeConfig(body string) {
	if err := os.WriteFile(filepath.Join(h.dir, "config"), []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func TestAddSkipsWhatItShould(t *testing.T) {
	h := newHarness(t)
	h.writeConfig("ignore = /^secret-cmd/\n")
	for _, cmd := range []string{
		"", "   ", "\t\n",
		" echo leading-space",
		"\techo leading-tab",
		"export API_KEY=deadbeef",
		"secret-cmd --do-it",
	} {
		if code := h.run(Add, "--cmd", cmd); code != 0 {
			t.Fatalf("add %q exited %d", cmd, code)
		}
	}
	if got := h.log(); got != "" {
		t.Fatalf("records were written for commands that should be skipped:\n%s", got)
	}
	if got := h.stderr.String(); got != "" {
		t.Fatalf("skipping is normal operation and must be silent, got: %s", got)
	}
	h.run(Add, "--cmd", "echo kept")
	if len(h.records()) != 1 {
		t.Fatalf("the ordinary command was not recorded")
	}
}

// TestAddAlwaysExitsZero is V7. add is the only subcommand a shell hook calls,
// and a non-zero status there breaks the prompt, or the shell under set -e.
func TestAddAlwaysExitsZero(t *testing.T) {
	t.Run("unwritable data directory", func(t *testing.T) {
		h := newHarness(t)
		ro := filepath.Join(h.dir, "readonly")
		if err := os.MkdirAll(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(ro, 0o700) })
		t.Setenv("ENDAP_DATA_DIR", filepath.Join(ro, "data"))
		if code := h.run(Add, "--cmd", "ls"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if h.stderr.Len() == 0 {
			t.Fatal("the failure was swallowed; nobody would find out history stopped being recorded")
		}
	})
	t.Run("broken config", func(t *testing.T) {
		h := newHarness(t)
		h.writeConfig("halflife = nonsense\nignore = /[unclosed/\nnot a setting\n")
		if code := h.run(Add, "--cmd", "ls"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if !strings.Contains(h.stderr.String(), "did not compile") {
			t.Fatalf("an ignore pattern that will not compile must be reported: %q", h.stderr.String())
		}
		if len(h.records()) != 1 {
			t.Fatal("a broken config stopped the record from being written")
		}
	})
	t.Run("bad flags", func(t *testing.T) {
		h := newHarness(t)
		if code := h.run(Add, "--nonsense"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if code := h.run(Add, "--cmd", "ls", "stray-positional"); code != 0 {
			t.Fatalf("exit %d", code)
		}
	})
	t.Run("unwritable log file", func(t *testing.T) {
		h := newHarness(t)
		h.run(Add, "--cmd", "ls")
		if err := os.Chmod(h.historyPath(), 0o400); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(h.historyPath(), 0o600) })
		if code := h.run(Add, "--cmd", "ls again"); code != 0 {
			t.Fatalf("exit %d", code)
		}
	})
}

func TestAddReadsLongCommandsFromStdin(t *testing.T) {
	h := newHarness(t)
	long := strings.Repeat("x", 150000)
	if code := Add(&Env{Stdin: strings.NewReader(long), Stdout: &h.stdout, Stderr: &h.stderr},
		[]string{"--cmd", "-", "--exit", "0"}); code != 0 {
		t.Fatal("exit code")
	}
	recs := h.records()
	if len(recs) != 1 || recs[0].Cmd != long {
		t.Fatalf("the long command did not survive the stdin path (%d records)", len(recs))
	}
}

// TestAddReadsTheCommandFromTheEnvironment covers how the hooks actually pass
// the command. An argument would be readable by every user on the host through
// /proc/<pid>/cmdline while endap runs, and for a shell builtin like
// "export TOKEN=..." that is an exposure endap creates where none existed.
func TestAddReadsTheCommandFromTheEnvironment(t *testing.T) {
	h := newHarness(t)
	t.Setenv("ENDAP_CMD", "echo from the environment")
	h.run(Add, "--cmd-env", "ENDAP_CMD", "--exit", "0")
	recs := h.records()
	if len(recs) != 1 || recs[0].Cmd != "echo from the environment" {
		t.Fatalf("got %v", recs)
	}
	// Everything the rules say about --cmd applies here too.
	t.Setenv("ENDAP_CMD", " echo leading space")
	h.run(Add, "--cmd-env", "ENDAP_CMD")
	t.Setenv("ENDAP_CMD", "export API_KEY=deadbeef")
	h.run(Add, "--cmd-env", "ENDAP_CMD")
	t.Setenv("ENDAP_CMD", "")
	h.run(Add, "--cmd-env", "ENDAP_CMD")
	if got := len(h.records()); got != 1 {
		t.Fatalf("got %d records, want 1", got)
	}
	// A variable that is not set at all is a broken hook, not an empty command.
	h.run(Add, "--cmd-env", "ENDAP_NOT_SET")
	if h.stderr.Len() == 0 {
		t.Fatal("an unset --cmd-env variable was accepted silently")
	}
	// Multi-line and NUL-free odd bytes survive the environment.
	t.Setenv("ENDAP_CMD", "echo 'multi\nline'\ttabbed")
	h.run(Add, "--cmd-env", "ENDAP_CMD")
	recs = h.records()
	if recs[len(recs)-1].Cmd != "echo 'multi\nline'\ttabbed" {
		t.Fatalf("got %q", recs[len(recs)-1].Cmd)
	}
}

func TestAddRejectsBothCmdForms(t *testing.T) {
	h := newHarness(t)
	t.Setenv("ENDAP_CMD", "from env")
	h.run(Add, "--cmd", "from flag", "--cmd-env", "ENDAP_CMD")
	if len(h.records()) != 0 {
		t.Fatal("a record was written despite contradictory flags")
	}
	if !strings.Contains(h.stderr.String(), "cannot both be given") {
		t.Fatalf("stderr = %q", h.stderr.String())
	}
}

// TestAddStdinIsVerbatim: the hooks write the command with no trailing newline,
// so trimming one would drop a newline the user really typed.
func TestAddStdinIsVerbatim(t *testing.T) {
	h := newHarness(t)
	const cmd = "echo one\necho two\n"
	if code := Add(&Env{Stdin: strings.NewReader(cmd), Stdout: &h.stdout, Stderr: &h.stderr},
		[]string{"--cmd", "-"}); code != 0 {
		t.Fatal("exit code")
	}
	recs := h.records()
	if len(recs) != 1 || recs[0].Cmd != cmd {
		t.Fatalf("got %q, want %q", recs[0].Cmd, cmd)
	}
}

func TestAddRecordsTheStartTimeItIsGiven(t *testing.T) {
	h := newHarness(t)
	h.run(Add, "--cmd", "sleep 1", "--ts", "1756339200123", "--dur", "1002", "--exit", "0", "--sess", "abc-1", "--sh", "zsh")
	r := h.records()[0]
	if r.TS != 1756339200123 {
		t.Fatalf("ts = %d; add runs after the command, so the start time has to come from the hook", r.TS)
	}
	if !r.HasDur || r.Dur != 1002 || !r.HasExit || r.Exit != 0 || r.Sess != "abc-1" || r.Sh != "zsh" {
		t.Fatalf("got %+v", r)
	}
	if r.Host != "testhost" {
		t.Fatalf("host = %q", r.Host)
	}
}

func TestAddRejectsAMalformedSessionId(t *testing.T) {
	h := newHarness(t)
	h.run(Add, "--cmd", "ls", "--sess", "../../etc/passwd")
	r := h.records()[0]
	if r.Sess != "" {
		t.Fatalf("sess = %q, want it dropped", r.Sess)
	}
	if h.stderr.Len() == 0 {
		t.Fatal("dropping the session id was not reported")
	}
}

func TestListOutput(t *testing.T) {
	h := newHarness(t)
	// Explicit timestamps: four adds in a row can land in the same
	// millisecond, and the default ordering is the one thing here that reads
	// them.
	now := time.Now().UnixMilli()
	at := func(secondsAgo int64) string { return strconv.FormatInt(now-secondsAgo*1000, 10) }
	h.run(Add, "--cmd", "git status", "--cwd", "/project", "--exit", "0", "--ts", at(40))
	h.run(Add, "--cmd", "git status", "--cwd", "/project", "--exit", "0", "--ts", at(30))
	h.run(Add, "--cmd", "echo a\nb", "--cwd", "/other", "--exit", "0", "--ts", at(20))
	h.run(Add, "--cmd", "make test", "--cwd", "/other", "--exit", "1", "--ts", at(10))

	code := h.run(List, "--cwd", "/project")
	if code != 0 {
		t.Fatalf("list exited %d", code)
	}
	out := h.stdout.String()
	if !strings.HasSuffix(out, "\x00") {
		t.Fatal("--print0 is the default and was not applied")
	}
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	// Newest first by default, even with --cwd naming the directory the
	// twice-run command was boosted for.
	if fields[0] != "make test" {
		t.Fatalf("the default order is not newest first: %q", fields)
	}
	if !strings.Contains(out, "echo a\nb") {
		t.Fatal("the multi-line command was lost")
	}

	h.run(List, "--sort", "rank", "--cwd", "/project", "--print0=false")
	if got, _, _ := strings.Cut(h.stdout.String(), "\n"); got != "git status" {
		t.Fatalf("--sort rank did not put the boosted, most-run command first: %q", got)
	}

	if code := h.run(List, "--sort", "nonsense"); code != 2 {
		t.Fatalf("an unknown --sort exited %d, want 2", code)
	}

	h.run(List, "--print0=false")
	if strings.Contains(h.stdout.String(), "\x00") {
		t.Fatal("--print0=false still emitted NULs")
	}

	h.run(List, "--sort", "rank", "--format", "tsv", "--print0=false", "--limit", "1")
	line := strings.TrimSuffix(h.stdout.String(), "\n")
	cols := strings.Split(line, "\t")
	if len(cols) != 5 {
		t.Fatalf("tsv row has %d columns: %q", len(cols), line)
	}
	if cols[1] != "2" {
		t.Fatalf("count column = %q", cols[1])
	}
}

func TestListSortComesFromTheConfig(t *testing.T) {
	// The widget calls plain `endap list`, so the config file is the only way
	// to opt a shell into the ranking.
	h := newHarness(t)
	now := time.Now().UnixMilli()
	at := func(secondsAgo int64) string { return strconv.FormatInt(now-secondsAgo*1000, 10) }
	h.run(Add, "--cmd", "old but frequent", "--ts", at(40))
	h.run(Add, "--cmd", "old but frequent", "--ts", at(30))
	h.run(Add, "--cmd", "new and rare", "--ts", at(10))

	h.writeConfig("sort = rank\n")
	h.run(List, "--print0=false")
	if got, _, _ := strings.Cut(h.stdout.String(), "\n"); got != "old but frequent" {
		t.Fatalf("sort = rank in the config was ignored: %q", got)
	}
	// An explicit flag still wins over it.
	h.run(List, "--sort", "recent", "--print0=false")
	if got, _, _ := strings.Cut(h.stdout.String(), "\n"); got != "new and rare" {
		t.Fatalf("--sort recent did not override the config: %q", got)
	}
}

func TestListFilters(t *testing.T) {
	h := newHarness(t)
	h.run(Add, "--cmd", "on host a", "--sess", "sess-a")
	t.Setenv("ENDAP_HOST", "otherhost")
	h.run(Add, "--cmd", "on host b", "--sess", "sess-b")

	h.run(List, "--host", "otherhost", "--print0=false")
	if got := strings.TrimSpace(h.stdout.String()); got != "on host b" {
		t.Fatalf("--host = %q", got)
	}
	h.run(List, "--session", "sess-a", "--print0=false")
	if got := strings.TrimSpace(h.stdout.String()); got != "on host a" {
		t.Fatalf("--session = %q", got)
	}
	h.run(List, "--since", "1h", "--print0=false")
	if n := len(strings.Fields(h.stdout.String())); n == 0 {
		t.Fatal("--since 1h dropped records written a moment ago")
	}
	h.run(List, "--no-dedupe", "--print0=false")
	if n := strings.Count(h.stdout.String(), "\n"); n != 2 {
		t.Fatalf("--no-dedupe produced %d lines", n)
	}
}

func TestExportIsUntouched(t *testing.T) {
	h := newHarness(t)
	h.run(Add, "--cmd", "ls")
	if err := os.WriteFile(h.historyPath(), []byte(h.log()+"a corrupt line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.run(Export)
	if h.stdout.String() != h.log() {
		t.Fatal("export changed the log")
	}
	if !strings.Contains(h.stdout.String(), "a corrupt line") {
		t.Fatal("export dropped a line endap cannot parse; it is the backup path")
	}
}

func TestCompactIsADryRunByDefault(t *testing.T) {
	h := newHarness(t)
	h.run(Add, "--cmd", "ls", "--ts", "1000")
	dup := h.log()
	if err := os.WriteFile(h.historyPath(), []byte(dup+dup+"corrupt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := h.log()

	h.run(Compact)
	if !strings.Contains(h.stdout.String(), "Nothing was changed") {
		t.Fatalf("compact did not report a dry run: %s", h.stdout.String())
	}
	if h.log() != before {
		t.Fatal("compact changed the log without --yes")
	}

	h.run(Compact, "--yes")
	recs := h.records()
	if len(recs) != 1 {
		t.Fatalf("got %d records after compact, want 1", len(recs))
	}
	rej, _ := os.ReadFile(store.RejPath(h.historyPath()))
	if !strings.Contains(string(rej), "corrupt") {
		t.Fatalf("the corrupt line was not parked: %q", rej)
	}
}

func TestCompactAppliesIgnoreAddedLater(t *testing.T) {
	h := newHarness(t)
	h.run(Add, "--cmd", "deploy --token abc")
	h.run(Add, "--cmd", "ls")
	h.writeConfig("ignore = /deploy/\n")
	h.run(Compact, "--yes")
	recs := h.records()
	if len(recs) != 1 || recs[0].Cmd != "ls" {
		t.Fatalf("got %v", recs)
	}
}

func TestForgetIsADryRunAndHidesCommands(t *testing.T) {
	h := newHarness(t)
	// Something the default ignore list does not catch, which is exactly the
	// case forget exists for.
	h.run(Add, "--cmd", "echo totallyfine | wall")
	h.run(Add, "--cmd", "ls")
	before := h.log()

	h.run(Forget, "totallyfine")
	out := h.stdout.String()
	if strings.Contains(out, "totallyfine") {
		t.Fatalf("the dry run printed the secret it was asked to delete: %s", out)
	}
	if !strings.Contains(out, "1 records") {
		t.Fatalf("the dry run did not report the match: %s", out)
	}
	if h.log() != before {
		t.Fatal("forget deleted without --yes")
	}

	h.run(Forget, "totallyfine", "--show-matches")
	if !strings.Contains(h.stdout.String(), "totallyfine") {
		t.Fatal("--show-matches did not show the match")
	}

	h.run(Forget, "totallyfine", "--yes")
	if strings.Contains(h.log(), "totallyfine") {
		t.Fatal("the record survived forget --yes")
	}
	if !strings.Contains(h.stdout.String(), "synced elsewhere") {
		t.Fatal("forget did not say what it cannot reach")
	}
}

func TestForgetReachesTheRejAndTmpFiles(t *testing.T) {
	h := newHarness(t)
	h.run(Add, "--cmd", "ls")
	dir := filepath.Dir(h.historyPath())
	rej := store.RejPath(h.historyPath())
	tmp := filepath.Join(dir, store.HistoryName+".tmp.999")
	for _, p := range []string{rej, tmp} {
		if err := os.WriteFile(p, []byte("a line holding hunter2\nanother line\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.run(Forget, "hunter2", "--yes")
	for _, p := range []string{rej, tmp} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if strings.Contains(string(b), "hunter2") {
			t.Fatalf("%s still holds the secret", filepath.Base(p))
		}
		if !strings.Contains(string(b), "another line") {
			t.Fatalf("%s lost a line that did not match", filepath.Base(p))
		}
	}
}

// TestForgetJudgesEveryFileTheSameWay: the leftover temporaries hold records in
// the same format as the log, so matching them by a different rule would delete
// different things from each.
func TestForgetJudgesEveryFileTheSameWay(t *testing.T) {
	h := newHarness(t)
	h.run(Add, "--cmd", "ls", "--sh", "zsh")
	record := strings.TrimSpace(h.log())
	tmp := filepath.Join(filepath.Dir(h.historyPath()), store.HistoryName+".tmp.7")
	if err := os.WriteFile(tmp, []byte(record+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// "zsh" appears only in the sh field, never in a command.
	h.run(Forget, "zsh", "--yes")
	if len(h.records()) != 1 {
		t.Fatal("forget matched the sh field in the log")
	}
	b, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("the temporary was removed: %v", err)
	}
	if !strings.Contains(string(b), `"cmd":"ls"`) {
		t.Fatalf("forget matched the sh field in the temporary: %q", b)
	}
}

func TestForgetMatchesCommandsNotMetadata(t *testing.T) {
	h := newHarness(t)
	h.run(Add, "--cmd", "ls", "--sh", "zsh")
	h.run(Add, "--cmd", "echo zsh is nice", "--sh", "zsh")
	h.run(Forget, "zsh", "--yes")
	recs := h.records()
	if len(recs) != 1 || recs[0].Cmd != "ls" {
		t.Fatalf("forget matched the sh field, not just the command: %v", recs)
	}
}

func TestForgetDeletesUnreadableLines(t *testing.T) {
	h := newHarness(t)
	h.run(Add, "--cmd", "ls")
	if err := os.WriteFile(h.historyPath(),
		[]byte(h.log()+"a broken line with hunter2 in it\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.run(Forget, "hunter2", "--yes")
	if strings.Contains(h.log(), "hunter2") {
		t.Fatal("the corrupt line holding a secret survived")
	}
	rej, _ := os.ReadFile(store.RejPath(h.historyPath()))
	if strings.Contains(string(rej), "hunter2") {
		t.Fatal("the corrupt line was parked in .rej instead of deleted")
	}
}

func TestMergeIsIdempotentAndOrdered(t *testing.T) {
	h := newHarness(t)
	h.run(Add, "--cmd", "local command", "--ts", "2000")
	remote := filepath.Join(h.dir, "remote.jsonl")
	body := `{"v":1,"id":"REMOTE1","ts":1000,"cmd":"remote old","host":"laptop"}
{"v":1,"id":"REMOTE2","ts":3000,"cmd":"remote new","host":"laptop"}
{"v":1,"id":"REMOTE2","ts":3000,"cmd":"remote new","host":"laptop"}
{"v":99,"id":"FUTURE","ts":4000,"cmd":"tomorrow","host":"laptop"}
not a record
`
	if err := os.WriteFile(remote, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	h.run(Merge, remote)
	lines := strings.Split(strings.TrimSuffix(h.stdout.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("merged output has %d lines:\n%s", len(lines), h.stdout.String())
	}
	if !strings.Contains(lines[0], "remote old") || !strings.Contains(lines[3], "tomorrow") {
		t.Fatalf("output is not in ts order:\n%s", h.stdout.String())
	}
	if strings.Contains(h.stdout.String(), "not a record") {
		t.Fatal("the corrupt line was merged into the output")
	}

	h.run(Merge, remote, "--in-place")
	first := h.log()
	h.run(Merge, remote, "--in-place")
	if h.log() != first {
		t.Fatal("merge is not idempotent")
	}
	if !strings.Contains(first, `"v":99`) {
		t.Fatal("the record from a newer endap did not survive the merge")
	}
}

// TestMergeInPlaceWithNoExistingLog is the first-sync case: a new machine has
// no history.jsonl yet. Rewrite is a no-op when there is nothing to rewrite,
// which is right for compact and forget and would silently drop everything
// being merged in here.
func TestMergeInPlaceWithNoExistingLog(t *testing.T) {
	h := newHarness(t)
	remote := filepath.Join(h.dir, "remote.jsonl")
	body := `{"v":1,"id":"R1","ts":1000,"cmd":"remote one","host":"laptop"}
{"v":1,"id":"R2","ts":2000,"cmd":"remote two","host":"laptop"}
`
	if err := os.WriteFile(remote, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.historyPath()); !os.IsNotExist(err) {
		t.Fatal("the test needs to start with no log")
	}
	if code := h.run(Merge, remote, "--in-place"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	recs := h.records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2; merge reported %q", len(recs), h.stderr.String())
	}
	// And it stays idempotent from there.
	h.run(Merge, remote, "--in-place")
	if got := len(h.records()); got != 2 {
		t.Fatalf("a second merge produced %d records", got)
	}
}

// TestMergeToStdoutTouchesNothing: without --in-place, merge is a read. Writing
// the corrupt lines to .rej would copy them out of the log while leaving them
// in it, and add the same lines again on every run.
func TestMergeToStdoutTouchesNothing(t *testing.T) {
	h := newHarness(t)
	h.run(Add, "--cmd", "local command")
	if err := os.WriteFile(h.historyPath(), []byte(h.log()+"a corrupt local line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := h.log()
	remote := filepath.Join(h.dir, "remote.jsonl")
	if err := os.WriteFile(remote, []byte("a corrupt remote line\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if code := h.run(Merge, remote); code != 0 {
			t.Fatalf("exit %d", code)
		}
	}
	if h.log() != before {
		t.Fatal("merge to stdout modified the log")
	}
	if _, err := os.Stat(store.RejPath(h.historyPath())); err == nil {
		rej, _ := os.ReadFile(store.RejPath(h.historyPath()))
		t.Fatalf("merge to stdout wrote to %s:\n%s", store.RejName, rej)
	}
	if !strings.Contains(h.stderr.String(), "corrupt lines skipped") {
		t.Fatalf("the summary claims something was moved: %q", h.stderr.String())
	}
}

// TestMergeToStdoutWithNoExistingLog: a missing log is an empty log, not an
// error. Merging someone else's file on a fresh machine has to work.
func TestMergeToStdoutWithNoExistingLog(t *testing.T) {
	h := newHarness(t)
	remote := filepath.Join(h.dir, "remote.jsonl")
	if err := os.WriteFile(remote,
		[]byte(`{"v":1,"id":"R1","ts":1000,"cmd":"remote one","host":"laptop"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.run(Merge, remote); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "remote one") {
		t.Fatalf("nothing was written to stdout: %q", h.stdout.String())
	}
	if _, err := os.Stat(h.historyPath()); !os.IsNotExist(err) {
		t.Fatal("a read created the log")
	}
}

func TestMergeAppliesIgnore(t *testing.T) {
	h := newHarness(t)
	remote := filepath.Join(h.dir, "remote.jsonl")
	if err := os.WriteFile(remote,
		[]byte(`{"v":1,"id":"R1","ts":1,"cmd":"export API_KEY=deadbeef","host":"laptop"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.run(Merge, remote)
	if strings.Contains(h.stdout.String(), "API_KEY") {
		t.Fatal("a secret walked back in through merge")
	}
}

func TestImportIsIdempotentAndKeepsCounts(t *testing.T) {
	h := newHarness(t)
	src := filepath.Join(h.dir, "bash_history")
	// No timestamps: every record gets ts=0, so a naive (ts, cmd, host) test
	// would collapse the three ls runs into one and lose the count.
	body := "ls\nls\nls\nmake test\nexport SECRET_TOKEN=abc\n"
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	h.run(Import, "bash", "--file", src)
	if got := len(h.records()); got != 4 {
		t.Fatalf("imported %d records, want 4", got)
	}
	h.run(Import, "bash", "--file", src)
	if got := len(h.records()); got != 4 {
		t.Fatalf("a second import added records: %d", got)
	}
	if strings.Contains(h.log(), "SECRET_TOKEN") {
		t.Fatal("import did not apply the ignore list")
	}
	// A file that has grown imports only the new part.
	if err := os.WriteFile(src, []byte(body+"ls\ngit push\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.run(Import, "bash", "--file", src)
	if got := len(h.records()); got != 6 {
		t.Fatalf("after the source grew there are %d records, want 6", got)
	}
}

func TestImportParsers(t *testing.T) {
	h := newHarness(t)
	t.Run("bash timestamps and continuations", func(t *testing.T) {
		src := filepath.Join(h.dir, "bh")
		os.WriteFile(src, []byte("#1700000000\nmake test\necho one \\\ntwo\n"), 0o600)
		got := parseBash(mustRead(t, src), "imported")
		if len(got) != 2 {
			t.Fatalf("got %d records: %+v", len(got), got)
		}
		if got[0].TS != 1700000000000 || got[0].Cmd != "make test" {
			t.Fatalf("got %+v", got[0])
		}
		if got[1].Cmd != "echo one \ntwo" {
			t.Fatalf("continuation not joined: %q", got[1].Cmd)
		}
	})
	t.Run("zsh extended history", func(t *testing.T) {
		src := filepath.Join(h.dir, "zh")
		os.WriteFile(src, []byte(": 1700000100:5;git push\nplain\n: bad;x\n"), 0o600)
		got := parseZsh(mustRead(t, src), "imported")
		if len(got) != 3 {
			t.Fatalf("got %d records: %+v", len(got), got)
		}
		if got[0].Cmd != "git push" || got[0].TS != 1700000100000 || !got[0].HasDur || got[0].Dur != 5000 {
			t.Fatalf("got %+v", got[0])
		}
		if got[1].Cmd != "plain" || got[1].TS != 0 {
			t.Fatalf("got %+v", got[1])
		}
		if got[2].Cmd != ": bad;x" {
			t.Fatalf("a line that only looks like a header was mangled: %q", got[2].Cmd)
		}
	})
	t.Run("zsh metafied bytes", func(t *testing.T) {
		// zsh writes the history file metafied and nothing in the file says so.
		// The escape covers NUL and all of 0x83-0xa2, and this string spans the
		// shapes that come out of that: U+30C3 (e3 83 83) escapes twice,
		// U+30C8 (e3 83 88) escapes a token-range byte on top of its Meta byte,
		// and U+30E2 (e3 83 a2) sits on the top of the range.
		//
		// metafy is not derived from unmetafyZsh -- it produces, byte for byte,
		// what zsh itself wrote when asked to save this exact string. That is
		// what stops the round trip from passing on a rule both halves get
		// wrong: with the range one byte too narrow the two stop agreeing.
		metafy := func(s string) []byte {
			var out []byte
			for _, b := range []byte(s) {
				if b == 0 || (b >= zshMeta && b <= 0xa2) {
					out = append(out, zshMeta, b^32)
					continue
				}
				out = append(out, b)
			}
			return out
		}
		const want = "mv スクリーンショット.png メモ/"
		src := filepath.Join(h.dir, "zh-meta")
		os.WriteFile(src, append(append([]byte(": 1700000200:0;"), metafy(want)...), '\n'), 0o600)
		got := parseZsh(mustRead(t, src), "imported")
		if len(got) != 1 {
			t.Fatalf("got %d records: %+v", len(got), got)
		}
		if got[0].Cmd != want {
			t.Fatalf("metafied command not decoded:\n got %q\nwant %q", got[0].Cmd, want)
		}
		// A file truncated in the middle of an escape keeps the trailing byte
		// rather than dropping it or reading past the end.
		if got := parseZsh([]byte("echo \x83"), "imported"); len(got) != 1 || got[0].Cmd != "echo \x83" {
			t.Fatalf("truncated escape: %+v", got)
		}
	})
	t.Run("fish", func(t *testing.T) {
		src := filepath.Join(h.dir, "fh")
		os.WriteFile(src, []byte("- cmd: git status\n  when: 1700000300\n  paths:\n    - a\n- cmd: two\\nlines\n  when: 1700000400\n"), 0o600)
		got := parseFish(mustRead(t, src), "imported")
		if len(got) != 2 {
			t.Fatalf("got %d records: %+v", len(got), got)
		}
		if got[0].Cmd != "git status" || got[0].TS != 1700000300000 {
			t.Fatalf("got %+v", got[0])
		}
		if got[1].Cmd != "two\nlines" {
			t.Fatalf("fish escape not decoded: %q", got[1].Cmd)
		}
	})
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDoctorReportsProblemsAndExitsNonZero(t *testing.T) {
	h := newHarness(t)
	h.run(Add, "--cmd", "ls")
	if err := os.WriteFile(h.historyPath(), []byte(h.log()+"corrupt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := h.run(Doctor)
	out := h.stdout.String()
	if code == 0 {
		t.Fatalf("doctor found a corrupt line and still exited 0:\n%s", out)
	}
	if !strings.Contains(out, "corrupt") {
		t.Fatalf("doctor did not mention the corrupt line:\n%s", out)
	}
	if !strings.Contains(out, "active ignore patterns") {
		t.Fatalf("doctor did not list the ignore patterns:\n%s", out)
	}
}

func TestDoctorIsQuietOnAHealthyInstall(t *testing.T) {
	h := newHarness(t)
	h.run(Add, "--cmd", "ls")
	code := h.run(Doctor)
	out := h.stdout.String()
	if strings.Contains(out, "ERROR") && !strings.Contains(out, "fzf") {
		t.Fatalf("doctor reported an error on a healthy install:\n%s", out)
	}
	if code != 0 && !strings.Contains(out, "fzf") {
		t.Fatalf("doctor exited %d on a healthy install:\n%s", code, out)
	}
}

func TestDoctorReportsStaleLocksAndTemporaries(t *testing.T) {
	h := newHarness(t)
	h.run(Add, "--cmd", "ls")
	dir := filepath.Dir(h.historyPath())
	os.WriteFile(store.LockPath(h.historyPath()), []byte("1\n"), 0o600)
	os.WriteFile(filepath.Join(dir, store.HistoryName+".tmp.42"), nil, 0o600)
	h.run(Doctor)
	out := h.stdout.String()
	if !strings.Contains(out, "stale lock") {
		t.Fatalf("doctor did not report the stale lock:\n%s", out)
	}
	if !strings.Contains(out, "leftover rewrite temporary") {
		t.Fatalf("doctor did not report the leftover temporary:\n%s", out)
	}
}

func TestStats(t *testing.T) {
	h := newHarness(t)
	h.run(Add, "--cmd", "ls", "--exit", "0", "--dur", "10")
	h.run(Add, "--cmd", "ls", "--exit", "0", "--dur", "20")
	h.run(Add, "--cmd", "false", "--exit", "1")
	h.run(Stats)
	out := h.stdout.String()
	for _, want := range []string{"records", "unique commands", "most run", "failed commands"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats is missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "2  ls") {
		t.Fatalf("stats did not count the repeats:\n%s", out)
	}
}
