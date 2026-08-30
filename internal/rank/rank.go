// Package rank merges duplicate commands and orders them, newest first by
// default and by frecency on request.
package rank

import (
	"math"
	"sort"
	"unicode/utf8"

	"github.com/shirou/endap/internal/config"
	"github.com/shirou/endap/internal/record"
)

// Entry is one command after duplicates have been merged.
type Entry struct {
	Cmd      string
	Count    int
	LastTs   int64
	LastCwd  string
	LastExit int
	HasExit  bool

	// CwdCount is how many of those runs happened in the directory passed to
	// the ranker, and CwdLastTs when the last of them was.
	//
	// The spec kept only LastCwd and boosted a command when it matched. One run
	// of "make test" somewhere else is enough to move LastCwd away and cancel
	// the boost for the fifty runs in the project directory, which leaves the
	// boost with nothing to do (D5).
	CwdCount  int
	CwdLastTs int64

	Score float64
}

// Ranker merges records and scores the result.
type Ranker struct {
	cfg    *config.Config
	cwd    string
	dedupe bool
	now    int64

	index map[string]int
	list  []Entry
}

// New returns a Ranker. cwd may be empty, in which case no directory boost is
// applied. When dedupe is false every record stays its own entry.
func New(cfg *config.Config, cwd string, dedupe bool, nowMillis int64) *Ranker {
	return &Ranker{
		cfg:    cfg,
		cwd:    cwd,
		dedupe: dedupe,
		now:    nowMillis,
		index:  make(map[string]int),
	}
}

// Add folds one record in. The record is not retained.
//
// Every Add must come before Entries: Entries sorts the entry slice in place,
// which leaves the command index pointing at the wrong rows.
func (r *Ranker) Add(rec *record.Record) {
	if !r.dedupe {
		e := Entry{Cmd: rec.Cmd, Count: 1, LastTs: rec.TS, LastCwd: rec.Cwd}
		if rec.HasExit {
			e.LastExit, e.HasExit = rec.Exit, true
		}
		if r.cwd != "" && rec.Cwd == r.cwd {
			e.CwdCount, e.CwdLastTs = 1, rec.TS
		}
		r.list = append(r.list, e)
		return
	}
	i, ok := r.index[rec.Cmd]
	if !ok {
		i = len(r.list)
		r.index[rec.Cmd] = i
		r.list = append(r.list, Entry{Cmd: rec.Cmd})
	}
	e := &r.list[i]
	e.Count++
	// Records arrive in file order, which is append order, but a merged log can
	// interleave hosts, so "last" is decided by timestamp rather than position.
	if rec.TS >= e.LastTs {
		e.LastTs = rec.TS
		e.LastCwd = rec.Cwd
		e.LastExit, e.HasExit = rec.Exit, rec.HasExit
	}
	if r.cwd != "" && rec.Cwd == r.cwd {
		e.CwdCount++
		if rec.TS > e.CwdLastTs {
			e.CwdLastTs = rec.TS
		}
	}
}

// Recent returns the entries newest first: the ordering Ctrl-R has in bash,
// zsh, fish, fzf and atuin, and the one endap uses unless asked for Ranked.
//
// Scores are still filled in because the tsv format prints them, but they do
// not affect the order. Ties keep the order the records arrived in, which
// leaves imported records that carry no timestamp of their own -- they all
// share ts=0 -- in the order the imported file had them.
//
// It sorts in place and invalidates the index, so no Add may follow it.
func (r *Ranker) Recent() []Entry {
	r.scoreAll()
	sort.SliceStable(r.list, func(a, b int) bool {
		return r.list[a].LastTs > r.list[b].LastTs
	})
	return r.list
}

// Ranked scores everything added so far and returns it in frecency order.
//
// It sorts in place and invalidates the index, so no Add may follow it.
func (r *Ranker) Ranked() []Entry {
	r.scoreAll()
	sort.SliceStable(r.list, func(a, b int) bool {
		x, y := &r.list[a], &r.list[b]
		if x.Score != y.Score {
			return x.Score > y.Score
		}
		return x.LastTs > y.LastTs
	})
	return r.list
}

func (r *Ranker) scoreAll() {
	for i := range r.list {
		r.list[i].Score = r.score(&r.list[i])
	}
}

// Millisecond spans of the recency bands.
const (
	hourMillis = int64(60 * 60 * 1000)
	dayMillis  = 24 * hourMillis
	weekMillis = 7 * dayMillis
)

// recencyMultiplier is zoxide's aging curve, moved from directories to
// commands: four bands spanning a factor of sixteen inside a single week.
//
// It replaced exponential decay with a half-life. A half-life long enough to
// keep a month-old command reachable -- 30 days was the first value -- decays
// by 2.3% in a day, which leaves the time term a near-constant next to
// log(count+1) and the ranking as frequency wearing frecency's name. A step
// function is cruder, but it spends the whole spread inside the week a shell
// session actually lives in.
func recencyMultiplier(ageMillis int64) float64 {
	switch {
	// A record from the future -- clocks disagree across synced hosts -- has a
	// negative age and lands in this band, which makes it new rather than
	// arbitrarily valuable.
	case ageMillis < hourMillis:
		return 4
	case ageMillis < dayMillis:
		return 2
	case ageMillis < weekMillis:
		return 0.5
	default:
		return 0.25
	}
}

// score is log(count+1) weighted by recency, with the three corrections.
func (r *Ranker) score(e *Entry) float64 {
	s := math.Log(float64(e.Count)+1) * recencyMultiplier(r.now-e.LastTs)
	if e.HasExit && e.LastExit != 0 {
		s *= r.cfg.FailPenalty
	}
	if e.CwdCount > 0 {
		s *= r.cfg.CwdBoost
	}
	// Characters, not bytes. On len() a one-character command like "猫" is
	// three bytes and escapes the penalty's intent in the wrong direction,
	// while a two-character one is six and escapes it in the other.
	if utf8.RuneCountInString(e.Cmd) <= r.cfg.ShortLen {
		s *= r.cfg.ShortPenalty
	}
	return s
}
