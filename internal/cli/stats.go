package cli

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/shirou/endap/internal/record"
	"github.com/shirou/endap/internal/store"
)

// StatsUsage documents the stats subcommand.
const StatsUsage = "stats [--top <n>]"

// Stats summarises the log.
func Stats(env *Env, args []string) int {
	fs := newFlagSet(env, "stats", StatsUsage)
	top := fs.Int("top", 10, "how many of the most-run commands to list")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	path, err := historyPath()
	if err != nil {
		env.errf("%v", err)
		return 1
	}

	byCmd := map[string]int{}
	byHost := map[string]int{}
	bySh := map[string]int{}
	var first, last int64
	var totalDur int64
	durCount := 0
	failed := 0

	st, err := store.WalkFile(path, func(r *record.Record) {
		byCmd[r.Cmd]++
		byHost[r.Host]++
		if r.Sh != "" {
			bySh[r.Sh]++
		}
		if r.TS > 0 && (first == 0 || r.TS < first) {
			first = r.TS
		}
		if r.TS > last {
			last = r.TS
		}
		if r.HasDur {
			totalDur += r.Dur
			durCount++
		}
		if r.HasExit && r.Exit != 0 {
			failed++
		}
	})
	if err != nil {
		env.errf("cannot read %s: %v", path, err)
		return 1
	}

	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "records\t%d\n", st.Records)
	fmt.Fprintf(w, "unique commands\t%d\n", len(byCmd))
	if st.Corrupt > 0 {
		fmt.Fprintf(w, "corrupt lines\t%d\n", st.Corrupt)
	}
	if st.Unknown > 0 {
		fmt.Fprintf(w, "newer-schema records\t%d\n", st.Unknown)
	}
	if first > 0 {
		fmt.Fprintf(w, "first record\t%s\n", time.UnixMilli(first).Format(time.RFC3339))
		fmt.Fprintf(w, "last record\t%s\n", time.UnixMilli(last).Format(time.RFC3339))
	}
	if st.Records > 0 {
		fmt.Fprintf(w, "failed commands\t%d (%.1f%%)\n", failed, 100*float64(failed)/float64(st.Records))
	}
	if durCount > 0 {
		fmt.Fprintf(w, "mean duration\t%s\n", (time.Duration(totalDur/int64(durCount)) * time.Millisecond).Round(time.Millisecond))
	}
	if info, err := os.Stat(path); err == nil {
		fmt.Fprintf(w, "log size\t%.1f MiB\n", float64(info.Size())/(1<<20))
	}
	writeCounts(w, "hosts", byHost)
	writeCounts(w, "shells", bySh)
	w.Flush()

	if *top > 0 && len(byCmd) > 0 {
		fmt.Fprintf(env.Stdout, "\nmost run:\n")
		type pair struct {
			cmd string
			n   int
		}
		pairs := make([]pair, 0, len(byCmd))
		for c, n := range byCmd {
			pairs = append(pairs, pair{c, n})
		}
		sort.Slice(pairs, func(i, j int) bool {
			if pairs[i].n != pairs[j].n {
				return pairs[i].n > pairs[j].n
			}
			return pairs[i].cmd < pairs[j].cmd
		})
		if len(pairs) > *top {
			pairs = pairs[:*top]
		}
		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		for _, p := range pairs {
			fmt.Fprintf(tw, "  %d\t%s\n", p.n, sanitizeField(p.cmd))
		}
		tw.Flush()
	}
	return 0
}

func writeCounts(w *tabwriter.Writer, label string, m map[string]int) {
	if len(m) == 0 {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	first := true
	for _, k := range keys {
		name := ""
		if first {
			name = label
			first = false
		}
		fmt.Fprintf(w, "%s\t%s: %d\n", name, k, m[k])
	}
}
