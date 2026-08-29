package cli

import (
	"bufio"
	"fmt"
	"strings"
	"time"

	"github.com/shirou/endap/internal/config"
	"github.com/shirou/endap/internal/rank"
	"github.com/shirou/endap/internal/record"
	"github.com/shirou/endap/internal/store"
)

// ListUsage documents the list subcommand.
const ListUsage = "list [--cwd <path>] [--host <name>] [--session <id>] [--limit <n>] " +
	"[--since <dur>] [--no-dedupe] [--format cmd|tsv] [--print0=false]"

// List prints the merged, ranked history.
//
// This is the only hot read path: it runs on every Ctrl-R, which is why the log
// is parsed by hand rather than through encoding/json.
func List(env *Env, args []string) int {
	fs := newFlagSet(env, "list", ListUsage)
	cwd := fs.String("cwd", "", "boost commands that were run in this directory")
	host := fs.String("host", "", "only records from this host")
	session := fs.String("session", "", "only records from this session")
	since := fs.String("since", "", "only records newer than this duration, e.g. 7d")
	limit := fs.Int("limit", 0, "stop after n entries (0 = no limit)")
	noDedupe := fs.Bool("no-dedupe", false, "keep every record instead of merging duplicates")
	format := fs.String("format", "cmd", "output format: cmd or tsv")
	print0 := fs.Bool("print0", true, "separate entries with NUL instead of newline")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *format != "cmd" && *format != "tsv" {
		env.errf("unknown --format %q (want cmd or tsv)", *format)
		return 2
	}

	var minTs int64
	if *since != "" {
		d, err := config.ParseDuration(*since)
		if err != nil {
			env.errf("%v", err)
			return 2
		}
		minTs = time.Now().Add(-d).UnixMilli()
	}

	cfg := loadConfig(env)
	path, err := historyPath()
	if err != nil {
		env.errf("%v", err)
		return 1
	}
	data, err := store.ReadFile(path)
	if err != nil {
		env.errf("cannot read %s: %v", path, err)
		return 1
	}

	r := rank.New(cfg, *cwd, !*noDedupe, time.Now().UnixMilli())
	store.Walk(data, func(rec *record.Record) {
		if *host != "" && rec.Host != *host {
			return
		}
		if *session != "" && rec.Sess != *session {
			return
		}
		if minTs != 0 && rec.TS < minTs {
			return
		}
		r.Add(rec)
	})
	entries := r.Entries()
	if *limit > 0 && len(entries) > *limit {
		entries = entries[:*limit]
	}

	sep := byte('\n')
	if *print0 {
		sep = 0
	}
	out := bufio.NewWriter(env.Stdout)
	for i := range entries {
		e := &entries[i]
		if *format == "tsv" {
			// cmd goes last so that a tab or a newline inside it cannot shift
			// the columns: fzf's --with-nth=5.. and --accept-nth=5.. both run
			// to the end of the line.
			fmt.Fprintf(out, "%.4f\t%d\t%s\t%s\t", e.Score, e.Count, formatTs(e.LastTs), sanitizeField(e.LastCwd))
		}
		out.WriteString(e.Cmd)
		out.WriteByte(sep)
	}
	if err := out.Flush(); err != nil {
		env.errf("cannot write the output: %v", err)
		return 1
	}
	return 0
}

// formatTs renders a timestamp for the tsv columns, which exist to be read by a
// person in the fzf preview pane. Machine-readable output is what export is for.
func formatTs(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
}

// sanitizeField keeps a metadata column from breaking the tab layout. Only cmd,
// the last column, is allowed to contain tabs and newlines.
func sanitizeField(s string) string {
	if !strings.ContainsAny(s, "\t\n\r") {
		return s
	}
	return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
}
