package rank

import (
	"testing"
	"time"

	"github.com/shirou/endap/internal/config"
	"github.com/shirou/endap/internal/record"
)

const day = int64(24 * 60 * 60 * 1000)

func rec(cmd, cwd string, ageDays int64, exit int, now int64) record.Record {
	r := record.Record{V: record.Version, Cmd: cmd, Cwd: cwd, TS: now - ageDays*day, Host: "h"}
	r.SetExit(exit)
	return r
}

func order(entries []Entry) []string {
	out := make([]string, len(entries))
	for i := range entries {
		out[i] = entries[i].Cmd
	}
	return out
}

func TestFrequencyIsLogarithmic(t *testing.T) {
	// 500 runs of ls must not always bury a single hard-won ffmpeg line.
	now := time.Now().UnixMilli()
	r := New(config.Defaults(), "", true, now)
	for range 500 {
		x := rec("ls -la", "/tmp", 20, 0, now)
		r.Add(&x)
	}
	ffmpeg := rec("ffmpeg -i in.mkv -c:v libx264 -crf 18 out.mp4", "/tmp", 7, 0, now)
	r.Add(&ffmpeg)
	got := order(r.Entries())
	if got[0] != "ls -la" {
		t.Fatalf("order = %v", got)
	}
	e := r.Entries()
	ratio := e[0].Score / e[1].Score
	// Counted linearly the ratio would be in the hundreds. The logarithm is
	// what keeps a one-off within reach of a command run all day.
	if ratio > 20 {
		t.Fatalf("500 runs outweigh one recent run by %.1fx; the log is not damping frequency", ratio)
	}
}

// TestCwdBoostSurvivesOneRunElsewhere is D5. With only lastCwd to go on, a
// single run in another directory cancels the boost for fifty runs in this one.
func TestCwdBoostSurvivesOneRunElsewhere(t *testing.T) {
	now := time.Now().UnixMilli()
	r := New(config.Defaults(), "/project", true, now)
	for range 50 {
		x := rec("make test", "/project", 1, 0, now)
		r.Add(&x)
	}
	// The most recent run of the same command happened somewhere else.
	elsewhere := rec("make test", "/tmp", 0, 0, now)
	r.Add(&elsewhere)
	other := rec("make lint", "/project", 1, 0, now)
	r.Add(&other)

	entries := r.Entries()
	var maketest *Entry
	for i := range entries {
		if entries[i].Cmd == "make test" {
			maketest = &entries[i]
		}
	}
	if maketest == nil {
		t.Fatal("make test is missing")
	}
	if maketest.LastCwd != "/tmp" {
		t.Fatalf("LastCwd = %q, want /tmp", maketest.LastCwd)
	}
	if maketest.CwdCount != 50 {
		t.Fatalf("CwdCount = %d, want 50", maketest.CwdCount)
	}
	if entries[0].Cmd != "make test" {
		t.Fatalf("the boost was cancelled by one run elsewhere: %v", order(entries))
	}
}

func TestFailPenaltyAndShortPenalty(t *testing.T) {
	now := time.Now().UnixMilli()
	cfg := config.Defaults()

	r := New(cfg, "", true, now)
	ok := rec("command-a", "", 0, 0, now)
	bad := rec("command-b", "", 0, 1, now)
	r.Add(&ok)
	r.Add(&bad)
	e := r.Entries()
	if e[0].Cmd != "command-a" {
		t.Fatalf("the failed command was not penalised: %v", order(e))
	}
	if got := e[1].Score / e[0].Score; got < 0.49 || got > 0.51 {
		t.Fatalf("fail penalty is %.3f, want 0.5", got)
	}

	r = New(cfg, "", true, now)
	short := rec("ls", "", 0, 0, now)
	long := rec("ls -la", "", 0, 0, now)
	r.Add(&short)
	r.Add(&long)
	e = r.Entries()
	if e[0].Cmd != "ls -la" {
		t.Fatalf("the short command was not penalised: %v", order(e))
	}
}

func TestShortPenaltyCountsCharacters(t *testing.T) {
	// The spec says "3 characters or shorter". Counting bytes would penalise
	// "猫" (one character, three bytes) and spare "日本語です" (five characters,
	// fifteen bytes), which is the rule inverted for anyone not typing ASCII.
	now := time.Now().UnixMilli()
	cfg := config.Defaults()
	for _, c := range []struct {
		cmd   string
		short bool
	}{
		{"ls", true}, {"gst", true}, {"ls -l", false},
		{"猫", true}, {"日本", true}, {"日本語", true}, {"日本語で", false},
	} {
		r := New(cfg, "", true, now)
		x := rec(c.cmd, "", 0, 0, now)
		r.Add(&x)
		baseline := New(cfg, "", true, now)
		y := rec("a command that is not short", "", 0, 0, now)
		baseline.Add(&y)

		got := r.Entries()[0].Score
		want := baseline.Entries()[0].Score
		penalised := got < want*0.9
		if penalised != c.short {
			t.Errorf("%q: penalised=%v, want %v (score %.4f vs %.4f)", c.cmd, penalised, c.short, got, want)
		}
	}
}

func TestRecencyDecay(t *testing.T) {
	now := time.Now().UnixMilli()
	cfg := config.Defaults() // halflife 30d
	r := New(cfg, "", true, now)
	fresh := rec("fresh command", "", 0, 0, now)
	old := rec("older command", "", 30, 0, now)
	r.Add(&fresh)
	r.Add(&old)
	e := r.Entries()
	if e[0].Cmd != "fresh command" {
		t.Fatalf("order = %v", order(e))
	}
	if got := e[1].Score / e[0].Score; got < 0.49 || got > 0.51 {
		t.Fatalf("one half-life decayed to %.3f, want 0.5", got)
	}
}

func TestFutureTimestampsDoNotWin(t *testing.T) {
	// Clocks disagree across synced hosts. A record from tomorrow should rank
	// as new, not as infinitely valuable.
	now := time.Now().UnixMilli()
	cfg := config.Defaults()
	r := New(cfg, "", true, now)
	future := rec("future", "", -10, 0, now)
	present := rec("present", "", 0, 0, now)
	r.Add(&future)
	r.Add(&present)
	e := r.Entries()
	if e[0].Score != e[1].Score {
		t.Fatalf("a future record scored differently: %+v", e)
	}
}

func TestNoDedupeKeepsEveryRecord(t *testing.T) {
	now := time.Now().UnixMilli()
	r := New(config.Defaults(), "", false, now)
	for range 3 {
		x := rec("ls", "/tmp", 0, 0, now)
		r.Add(&x)
	}
	if got := len(r.Entries()); got != 3 {
		t.Fatalf("got %d entries, want 3", got)
	}
}

func TestLastFieldsFollowTheNewestRecord(t *testing.T) {
	// A merged log can hand records over out of order, so "last" is by
	// timestamp and not by position in the file.
	now := time.Now().UnixMilli()
	r := New(config.Defaults(), "", true, now)
	newer := rec("cmd", "/new", 0, 1, now)
	older := rec("cmd", "/old", 5, 0, now)
	r.Add(&newer)
	r.Add(&older)
	e := r.Entries()[0]
	if e.LastCwd != "/new" || e.LastExit != 1 || e.Count != 2 {
		t.Fatalf("got %+v", e)
	}
}
