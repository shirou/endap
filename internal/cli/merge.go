package cli

import (
	"bufio"
	"fmt"
	"os"
	"sort"

	"github.com/shirou/endap/internal/record"
	"github.com/shirou/endap/internal/store"
)

// MergeUsage documents the merge subcommand.
const MergeUsage = "merge <file>... [--in-place]"

// mergeLine is one line on its way through a merge, with the sort key it needs.
type mergeLine struct {
	raw []byte
	ts  int64
	id  string
}

// Merge combines logs from several hosts.
//
// Records are deduplicated by id and ordered by ts, which makes the operation
// order-independent and repeatable: the id carries per-host randomness, so two
// hosts never mint the same one, and running merge twice changes nothing. That
// is what lets a git conflict be resolved by keeping both sides and merging.
//
// The result goes to stdout unless --in-place is given, so the destructive form
// has to be asked for.
func Merge(env *Env, args []string) int {
	fs := newFlagSet(env, "merge", MergeUsage)
	inPlace := fs.Bool("in-place", false, "replace the local log instead of writing to stdout")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) == 0 {
		env.errf("merge needs at least one file")
		return 2
	}
	cfg := loadConfig(env)
	path, err := historyPath()
	if err != nil {
		env.errf("%v", err)
		return 1
	}

	var extra []mergeLine
	var c counts
	d := newDedupe()
	// Records are identified by id here, not by (ts, cmd, host): that is what
	// makes a merge order-independent and repeatable.
	byID := map[string]bool{}
	var rej [][]byte
	var rec record.Record

	// take decides what happens to one line from any source.
	take := func(line []byte) (mergeLine, bool) {
		kind := store.Classify(line, &rec)
		if kind == store.KindCorrupt {
			c.corrupt++
			rej = append(rej, cloneLine(line))
			return mergeLine{}, false
		}
		if kind == store.KindRecord && cfg.Ignore.Match(rec.Cmd) {
			// A merge is an intake point like add and import, so the ignore
			// list applies here too. Otherwise a secret filtered out on this
			// host walks back in from another one.
			c.ignored++
			return mergeLine{}, false
		}
		if rec.ID != "" {
			if byID[rec.ID] {
				c.duplicates++
				return mergeLine{}, false
			}
			byID[rec.ID] = true
		} else if d.seenRaw(line) {
			c.duplicates++
			return mergeLine{}, false
		}
		if kind == store.KindUnknown {
			c.unknown++
		}
		c.kept++
		return mergeLine{raw: cloneLine(line), ts: rec.TS, id: rec.ID}, true
	}

	for _, f := range pos {
		data, err := store.ReadFile(f)
		if err != nil {
			env.errf("cannot read %s: %v", f, err)
			return 1
		}
		for _, line := range store.SplitLines(data) {
			if len(line) == 0 {
				continue
			}
			if ml, ok := take(line); ok {
				extra = append(extra, ml)
			}
		}
	}

	// build is called exactly once, by Rewrite or directly, and take carries
	// state across both the files above and the local log.
	build := func(lines [][]byte) ([][]byte, error) {
		all := make([]mergeLine, 0, len(lines)+len(extra))
		for _, line := range lines {
			if len(line) == 0 {
				continue
			}
			if ml, ok := take(line); ok {
				all = append(all, ml)
			}
		}
		all = append(all, extra...)
		sort.SliceStable(all, func(i, j int) bool {
			if all[i].ts != all[j].ts {
				return all[i].ts < all[j].ts
			}
			return all[i].id < all[j].id
		})
		out := make([][]byte, len(all))
		for i := range all {
			out[i] = all[i].raw
		}
		return out, nil
	}

	if *inPlace {
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			// Rewrite is a no-op when there is no log to rewrite, which is right
			// for compact and forget and wrong here: the records to merge in are
			// all in `extra`, and reporting success while writing nothing is how
			// a first sync onto a new machine loses everything it was given.
			out, err := build(nil)
			if err != nil {
				env.errf("%v", err)
				return 1
			}
			if err := store.AppendBatch(path, out); err != nil {
				env.errf("cannot write to %s: %v", path, err)
				return 1
			}
		} else if err := store.Rewrite(path, build); err != nil {
			env.errf("%v", err)
			return 1
		}
		// Only the in-place form takes the corrupt lines out of a file, so only
		// it has anywhere to put them. Doing this in the stdout form would copy
		// the local corrupt lines into .rej while leaving them in the log, and
		// grow .rej by the same lines on every run.
		if err := store.AppendRej(path, rej); err != nil {
			env.errf("cannot write %s: %v", store.RejName, err)
			return 1
		}
		fmt.Fprintf(env.Stderr, "merged %d records (%d duplicates, %d ignored, %d corrupt lines moved to %s, %d from a newer endap).\n",
			c.kept, c.duplicates, c.ignored, c.corrupt, store.RejName, c.unknown)
		return 0
	}

	local, err := store.ReadFile(path)
	if err != nil {
		env.errf("cannot read %s: %v", path, err)
		return 1
	}
	out, err := build(store.SplitLines(local))
	if err != nil {
		env.errf("%v", err)
		return 1
	}
	w := bufio.NewWriter(env.Stdout)
	for _, line := range out {
		w.Write(line)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		env.errf("cannot write the output: %v", err)
		return 1
	}
	fmt.Fprintf(env.Stderr, "merged %d records (%d duplicates, %d ignored, %d corrupt lines skipped, %d from a newer endap).\n",
		c.kept, c.duplicates, c.ignored, c.corrupt, c.unknown)
	return 0
}

// cloneLine copies a line out of the shared read buffer so it can outlive it.
func cloneLine(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
