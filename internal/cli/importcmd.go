package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/endap/internal/record"
	"github.com/shirou/endap/internal/store"
)

// ImportUsage documents the import subcommand.
const ImportUsage = "import <bash|zsh|fish|atuin|endap> [--file <path>] [--host <name>] [--dry-run]"

// ImportedHost is the host name given to records that came from another tool,
// which have no host of their own.
const ImportedHost = "imported"

// Import reads an existing history file into the log.
//
// Importing is idempotent, and it stays idempotent for entries with no
// timestamp. The spec's "(ts, cmd, host) already present" test collapses every
// repeat of a command from a plain ~/.bash_history into one record, because
// they all share ts=0 -- which throws away exactly the run counts that drive
// ranking. So the comparison is by multiplicity instead: if the source has a
// key n times and the log already has it m times, max(0, n-m) copies are added.
// Re-running an import adds nothing; importing a file that has grown adds only
// what grew.
func Import(env *Env, args []string) int {
	fs := newFlagSet(env, "import", ImportUsage)
	file := fs.String("file", "", "history file to read (defaults per source)")
	host := fs.String("host", ImportedHost, "host name to record")
	dryRun := fs.Bool("dry-run", false, "report what would be imported without writing")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		env.errf("import needs exactly one source (bash, zsh, fish, atuin, endap)")
		return 2
	}
	source := pos[0]

	cfg := loadConfig(env)
	path, err := historyPath()
	if err != nil {
		env.errf("%v", err)
		return 1
	}

	candidates, err := readSource(env, source, *file, *host)
	if err != nil {
		env.errf("%v", err)
		return 1
	}

	// Multiplicity of every (ts, cmd, host) already in the log.
	have := map[string]int{}
	if _, err := store.WalkFile(path, func(r *record.Record) {
		have[dupKey(r)]++
	}); err != nil {
		env.errf("cannot read %s: %v", path, err)
		return 1
	}

	var lines [][]byte
	skippedDup, skippedIgnore := 0, 0
	for i := range candidates {
		rec := &candidates[i]
		if !Recordable(cfg, rec.Cmd) {
			// The ignore list applies here above all: a plain ~/.bash_history is
			// where years of accumulated secrets live, and this is the one
			// moment they would all be copied in at once.
			skippedIgnore++
			continue
		}
		k := dupKey(rec)
		if have[k] > 0 {
			have[k]--
			skippedDup++
			continue
		}
		rec.V = record.Version
		rec.ID = record.NewID(rec.TS)
		data, err := record.Encode(rec)
		if err != nil {
			env.errf("cannot encode a record: %v", err)
			return 1
		}
		lines = append(lines, data)
	}

	if *dryRun {
		fmt.Fprintf(env.Stdout, "would import %d records from %s (%d already present, %d skipped by ignore).\n",
			len(lines), source, skippedDup, skippedIgnore)
		return 0
	}
	if err := store.AppendBatch(path, lines); err != nil {
		env.errf("cannot write to %s: %v", path, err)
		return 1
	}
	fmt.Fprintf(env.Stdout, "imported %d records from %s (%d already present, %d skipped by ignore).\n",
		len(lines), source, skippedDup, skippedIgnore)
	return 0
}

func readSource(env *Env, source, file, host string) ([]record.Record, error) {
	def := func(rel ...string) string {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(append([]string{home}, rel...)...)
	}
	switch source {
	case "bash":
		if file == "" {
			file = def(".bash_history")
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		return parseBash(data, host), nil
	case "zsh":
		if file == "" {
			file = def(".zsh_history")
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		return parseZsh(data, host), nil
	case "fish":
		if file == "" {
			file = def(".local", "share", "fish", "fish_history")
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		return parseFish(data, host), nil
	case "endap":
		if file == "" {
			return nil, fmt.Errorf("import endap needs --file")
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		return parseEndap(data), nil
	case "atuin":
		return readAtuin(env, file, host)
	default:
		return nil, fmt.Errorf("unknown source %q (want bash, zsh, fish, atuin or endap)", source)
	}
}

// joinContinuations merges lines that the shell wrote as a continuation, which
// is how both bash and zsh store a command containing a newline.
func joinContinuations(lines []string) []string {
	var out []string
	var pending strings.Builder
	open := false
	for _, l := range lines {
		if strings.HasSuffix(l, `\`) && !strings.HasSuffix(l, `\\`) {
			pending.WriteString(strings.TrimSuffix(l, `\`))
			pending.WriteString("\n")
			open = true
			continue
		}
		if open {
			pending.WriteString(l)
			out = append(out, pending.String())
			pending.Reset()
			open = false
			continue
		}
		out = append(out, l)
	}
	if open {
		out = append(out, strings.TrimSuffix(pending.String(), "\n"))
	}
	return out
}

func splitLines(data []byte) []string {
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// parseBash reads ~/.bash_history. With HISTTIMEFORMAT set, bash writes a
// "#<epoch>" line before each command; without it there are no timestamps at
// all, and those records get ts=0 and rank as the oldest thing in the log.
func parseBash(data []byte, host string) []record.Record {
	var out []record.Record
	var ts int64
	for _, line := range joinContinuations(splitLines(data)) {
		if strings.HasPrefix(line, "#") {
			if n, err := strconv.ParseInt(strings.TrimSpace(line[1:]), 10, 64); err == nil {
				ts = n * 1000
				continue
			}
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, record.Record{TS: ts, Cmd: line, Host: host, Sh: "bash"})
		ts = 0
	}
	return out
}

// zshMeta is zsh's Meta byte.
//
// zsh writes the history file in metafied form: a byte it treats as special --
// Meta itself, NUL, and the token range 0x84-0x9e above it -- is stored as Meta
// followed by that byte XORed with 32. Nothing in the file marks a line as
// escaped.
const zshMeta = 0x83

// unmetafyZsh undoes that escaping.
//
// Skipping it corrupts every command holding a character whose UTF-8 contains
// one of those bytes, which is most non-ASCII text: U+30EA is e3 83 aa in UTF-8,
// reaches the file as e3 83 a3 aa, and read literally comes back as U+30E3
// followed by a stray byte. zsh unmetafies on the way in, so the file is not
// damaged -- only a reader that takes its bytes at face value is.
//
// Undoing it before the file is split into lines is safe: the second byte of an
// escape is always 0x20, 0x80, or 0xa3-0xbe, so an escape can neither hide a
// newline nor hide the backslash joinContinuations looks for, and unmetafying
// cannot produce one either.
func unmetafyZsh(b []byte) []byte {
	i := bytes.IndexByte(b, zshMeta)
	if i < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	out = append(out, b[:i]...)
	for ; i < len(b); i++ {
		// A Meta with nothing after it is a truncated file rather than an
		// escape, so the byte is kept instead of silently dropped.
		if b[i] == zshMeta && i+1 < len(b) {
			i++
			out = append(out, b[i]^32)
			continue
		}
		out = append(out, b[i])
	}
	return out
}

// parseZsh reads ~/.zsh_history. With EXTENDED_HISTORY set, zsh writes a
// ": <ts>:<elapsed>;" header before each command; without it the line is the
// command alone and the record gets ts=0.
func parseZsh(data []byte, host string) []record.Record {
	var out []record.Record
	for _, line := range joinContinuations(splitLines(unmetafyZsh(data))) {
		rec := record.Record{Host: host, Sh: "zsh"}
		rest := line
		if strings.HasPrefix(line, ": ") {
			if semi := strings.Index(line, ";"); semi > 0 {
				meta := line[2:semi]
				if colon := strings.Index(meta, ":"); colon > 0 {
					if ts, err := strconv.ParseInt(strings.TrimSpace(meta[:colon]), 10, 64); err == nil {
						rec.TS = ts * 1000
						if elapsed, err := strconv.ParseInt(strings.TrimSpace(meta[colon+1:]), 10, 64); err == nil && elapsed > 0 {
							rec.SetDur(elapsed * 1000)
						}
						rest = line[semi+1:]
					}
				}
			}
		}
		if strings.TrimSpace(rest) == "" {
			continue
		}
		rec.Cmd = rest
		out = append(out, rec)
	}
	return out
}

// parseFish reads fish_history, which is YAML-shaped but not YAML: fish writes
// a fixed layout and escapes only backslashes and newlines in the command.
func parseFish(data []byte, host string) []record.Record {
	var out []record.Record
	var cur *record.Record
	flush := func() {
		if cur != nil && strings.TrimSpace(cur.Cmd) != "" {
			out = append(out, *cur)
		}
		cur = nil
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "- cmd: "):
			flush()
			cur = &record.Record{Cmd: unescapeFish(line[len("- cmd: "):]), Host: host, Sh: "fish"}
		case cur != nil && strings.HasPrefix(strings.TrimSpace(line), "when: "):
			v := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "when: "))
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				cur.TS = n * 1000
			}
		}
	}
	flush()
	return out
}

func unescapeFish(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte('\\')
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseEndap reads another endap log. Corrupt lines and records from a newer
// endap are skipped rather than rewritten: use merge for those.
func parseEndap(data []byte) []record.Record {
	var out []record.Record
	store.Walk(data, func(r *record.Record) {
		out = append(out, *r)
	})
	return out
}

// atuinFormat pins the column layout instead of relying on atuin's default,
// which is a display format and not a stable interface.
const atuinFormat = "{time}\t{exit}\t{duration}\t{directory}\t{command}"

// readAtuin imports from atuin.
//
// atuin keeps its history in a database, and endap does not embed one, so the
// data has to come through atuin itself. Either atuin is run with a pinned
// format, or the user produces the same output beforehand and passes --file:
//
//	atuin history list --format "{time}\t{exit}\t{duration}\t{directory}\t{command}" > atuin.tsv
//	endap import atuin --file atuin.tsv
//
// A command containing a newline cannot survive this line-oriented shape. That
// is a limit of the tab-separated intermediate, and it is in the README.
func readAtuin(env *Env, file, host string) ([]record.Record, error) {
	var data []byte
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		data = b
	} else {
		bin, err := exec.LookPath("atuin")
		if err != nil {
			return nil, fmt.Errorf("atuin is not on PATH; export its history and pass --file:\n" +
				"  atuin history list --format \"" + atuinFormat + "\" > atuin.tsv")
		}
		cmd := exec.Command(bin, "history", "list", "--format", atuinFormat)
		cmd.Stderr = env.Stderr
		b, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("running atuin history list: %w", err)
		}
		data = b
	}
	var out []record.Record
	for _, line := range splitLines(data) {
		fields := strings.SplitN(line, "\t", 5)
		if len(fields) < 5 {
			// Not the pinned format; treat the whole line as a command so
			// nothing is silently dropped.
			if strings.TrimSpace(line) != "" {
				out = append(out, record.Record{Cmd: line, Host: host, Sh: "atuin"})
			}
			continue
		}
		rec := record.Record{Cmd: fields[4], Cwd: fields[3], Host: host, Sh: "atuin"}
		if ts, ok := parseAtuinTime(fields[0]); ok {
			rec.TS = ts
		}
		if n, err := strconv.Atoi(strings.TrimSpace(fields[1])); err == nil {
			rec.SetExit(n)
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64); err == nil && n > 0 {
			// atuin reports duration in nanoseconds.
			rec.SetDur(n / 1e6)
		}
		if strings.TrimSpace(rec.Cmd) != "" {
			out = append(out, rec)
		}
	}
	return out, nil
}

// parseAtuinTime accepts either a Unix timestamp or one of the date layouts
// atuin prints, and gives up quietly rather than guessing wrong.
func parseAtuinTime(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 {
			return n, true
		}
		return n * 1000, true
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02 15:04:05 -0700 MST",
	} {
		if t, err := timeParse(layout, s); err == nil {
			return t, true
		}
	}
	return 0, false
}

func timeParse(layout, value string) (int64, error) {
	t, err := time.Parse(layout, value)
	if err != nil {
		return 0, err
	}
	return t.UnixMilli(), nil
}
