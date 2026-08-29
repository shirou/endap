package cli

import (
	"fmt"

	"github.com/shirou/endap/internal/record"
	"github.com/shirou/endap/internal/store"
)

// CompactUsage documents the compact subcommand.
const CompactUsage = "compact [--yes]"

// Compact rebuilds the log.
//
// It does exactly three things: drop exact duplicates, drop records that a
// later addition to the ignore list now matches, and park corrupt lines in
// history.jsonl.rej. It does not fold old records into aggregates -- two hosts
// compacting the same synced log would each produce an aggregate with its own
// id, and merge deduplicates by id, so the run counts would be counted twice
// with no way to tell.
//
// It is a dry run unless --yes is given. There is no .bak: a hard link cannot
// be re-created on the second compact, it pins the old inode so the disk is
// never freed, and it would keep a copy of exactly the secret an ignore pattern
// was just added to remove.
func Compact(env *Env, args []string) int {
	fs := newFlagSet(env, "compact", CompactUsage)
	yes := fs.Bool("yes", false, "rewrite the log instead of reporting what would change")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := loadConfig(env)
	path, err := historyPath()
	if err != nil {
		env.errf("%v", err)
		return 1
	}

	var c counts
	var rej [][]byte
	build := func(lines [][]byte) ([][]byte, error) {
		c, rej = counts{}, nil
		d := newDedupe()
		out := make([][]byte, 0, len(lines))
		var rec record.Record
		for _, line := range lines {
			switch store.Classify(line, &rec) {
			case store.KindCorrupt:
				c.corrupt++
				rej = append(rej, line)
			case store.KindUnknown:
				if d.seenRaw(line) {
					c.duplicates++
					continue
				}
				c.unknown++
				c.kept++
				out = append(out, line)
			default:
				if cfg.Ignore.Match(rec.Cmd) {
					c.ignored++
					continue
				}
				if d.seenRecord(&rec) {
					c.duplicates++
					continue
				}
				c.kept++
				out = append(out, line)
			}
		}
		return out, nil
	}

	if !*yes {
		data, err := store.ReadFile(path)
		if err != nil {
			env.errf("cannot read %s: %v", path, err)
			return 1
		}
		if _, err := build(store.SplitLines(data)); err != nil {
			env.errf("%v", err)
			return 1
		}
		fmt.Fprintf(env.Stdout,
			"would remove %d exact duplicates and %d records matching ignore, and move %d corrupt lines to %s.\n",
			c.duplicates, c.ignored, c.corrupt, store.RejName)
		fmt.Fprintf(env.Stdout, "%d records would remain (%d of them from a newer endap, passed through untouched).\n",
			c.kept, c.unknown)
		fmt.Fprintf(env.Stdout, "Nothing was changed. Re-run with --yes to apply.\n")
		return 0
	}

	if err := store.Rewrite(path, build); err != nil {
		env.errf("%v", err)
		return 1
	}
	if err := store.AppendRej(path, rej); err != nil {
		env.errf("cannot write %s: %v", store.RejName, err)
		return 1
	}
	fmt.Fprintf(env.Stdout, "removed %d duplicates and %d ignored records; %d corrupt lines moved to %s; %d records remain.\n",
		c.duplicates, c.ignored, c.corrupt, store.RejName, c.kept)
	return 0
}
