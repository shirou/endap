package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/shirou/endap/internal/record"
	"github.com/shirou/endap/internal/store"
)

// ForgetUsage documents the forget subcommand.
const ForgetUsage = "forget <regexp> [--yes] [--show-matches]"

// Forget deletes every record matching a pattern.
//
// It is the one rewrite that deliberately removes lines the parser cannot read:
// a corrupt line can hold a secret just as easily as a well-formed one, so the
// pattern is applied to the raw bytes as well as to the decoded command.
//
// The dry run prints counts and record ids and never the commands themselves.
// Showing "the first 20 characters" would not be a mask -- the first 20
// characters of "mysql -uroot -phunter2" are "mysql -uroot -phunte". Use
// --show-matches to see them on purpose.
//
// What forget can reach is limited, and the README says so: it removes records
// from this host's files. It cannot reach blocks already freed on disk, copies
// already synced to other hosts, or the user's own backups.
func Forget(env *Env, args []string) int {
	fs := newFlagSet(env, "forget", ForgetUsage)
	yes := fs.Bool("yes", false, "delete instead of reporting what would be deleted")
	show := fs.Bool("show-matches", false, "print the matching commands (they are hidden by default)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		env.errf("forget needs exactly one pattern")
		return 2
	}
	re, err := regexp.Compile(pos[0])
	if err != nil {
		env.errf("bad pattern: %v", err)
		return 2
	}
	path, err := historyPath()
	if err != nil {
		env.errf("%v", err)
		return 1
	}

	var c counts
	var rej [][]byte
	var ids []string
	var shown []string
	// A readable record is matched on its command text alone. Running the
	// pattern over the whole JSON line as well would make "forget zsh" delete
	// every record whose sh field says zsh, which is not what anyone typing it
	// means. A line the parser cannot read has no command to match, so there
	// the raw bytes are all there is -- and a corrupt line can hold a secret
	// just as easily as a well-formed one.
	//
	// The same rule is used for the rejected-lines file and the leftover
	// temporaries. They hold records in the same format, so judging them by a
	// different standard would delete different things from each.
	matches := func(line []byte) bool {
		var rec record.Record
		if store.Classify(line, &rec) == store.KindRecord {
			return re.MatchString(rec.Cmd)
		}
		return re.Match(line)
	}
	build := func(lines [][]byte) ([][]byte, error) {
		c, rej, ids, shown = counts{}, nil, nil, nil
		out := make([][]byte, 0, len(lines))
		var rec record.Record
		for _, line := range lines {
			kind := store.Classify(line, &rec)
			// A record from a newer endap parses, but its schema is not this
			// one's, so its command field is not trusted to be the command.
			parsed := kind == store.KindRecord
			if matches(line) {
				c.matched++
				if parsed && rec.ID != "" {
					ids = append(ids, rec.ID)
				} else {
					ids = append(ids, "(unparsed line)")
				}
				if parsed {
					shown = append(shown, rec.Cmd)
				} else {
					shown = append(shown, string(line))
				}
				continue
			}
			if kind == store.KindCorrupt {
				c.corrupt++
				rej = append(rej, line)
				continue
			}
			if kind == store.KindUnknown {
				c.unknown++
			}
			c.kept++
			out = append(out, line)
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
		aux, err := auxCounts(path, matches)
		if err != nil {
			env.errf("%v", err)
			return 1
		}
		fmt.Fprintf(env.Stdout, "%d records in %s match:\n", c.matched, store.HistoryName)
		for i, id := range ids {
			if *show {
				fmt.Fprintf(env.Stdout, "  %s  %q\n", id, shown[i])
			} else {
				fmt.Fprintf(env.Stdout, "  %s\n", id)
			}
		}
		for _, a := range aux {
			fmt.Fprintf(env.Stdout, "%d lines in %s match.\n", a.n, filepath.Base(a.path))
		}
		fmt.Fprintf(env.Stdout, "Nothing was changed. Re-run with --yes to delete.\n")
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
	// The rejected-lines file and any temporary left by an interrupted compact
	// are full of the same records, so a secret is not gone until they are
	// cleaned too.
	auxRemoved, err := filterAux(path, matches)
	if err != nil {
		env.errf("%v", err)
		return 1
	}
	fmt.Fprintf(env.Stdout, "deleted %d records from %s and %d lines from the other files; %d records remain.\n",
		c.matched, store.HistoryName, auxRemoved, c.kept)
	fmt.Fprintf(env.Stdout,
		"This removed the records from this host. Copies already synced elsewhere, your own backups, and freed disk blocks are out of reach.\n")
	return 0
}

type auxCount struct {
	path string
	n    int
}

// auxFiles lists the files besides history.jsonl that can hold the same records.
func auxFiles(history string) []string {
	var files []string
	if _, err := os.Stat(store.RejPath(history)); err == nil {
		files = append(files, store.RejPath(history))
	}
	if tmps, err := store.TmpFiles(history); err == nil {
		files = append(files, tmps...)
	}
	return files
}

func auxCounts(history string, matches func([]byte) bool) ([]auxCount, error) {
	var out []auxCount
	for _, p := range auxFiles(history) {
		data, err := store.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("cannot read %s: %w", p, err)
		}
		n := 0
		for _, line := range store.SplitLines(data) {
			if matches(line) {
				n++
			}
		}
		if n > 0 {
			out = append(out, auxCount{p, n})
		}
	}
	return out, nil
}

func filterAux(history string, matches func([]byte) bool) (int, error) {
	removed := 0
	for _, p := range auxFiles(history) {
		data, err := store.ReadFile(p)
		if err != nil {
			return removed, fmt.Errorf("cannot read %s: %w", p, err)
		}
		lines := store.SplitLines(data)
		kept := make([][]byte, 0, len(lines))
		n := 0
		for _, line := range lines {
			if matches(line) {
				n++
				continue
			}
			kept = append(kept, line)
		}
		if n == 0 {
			continue
		}
		removed += n
		if err := store.ReplaceFile(p, kept); err != nil {
			return removed, err
		}
	}
	return removed, nil
}
