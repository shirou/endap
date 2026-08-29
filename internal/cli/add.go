package cli

import (
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/shirou/endap/internal/config"
	"github.com/shirou/endap/internal/record"
	"github.com/shirou/endap/internal/store"
)

// sessPattern is the shape a session id must have to be recorded. The id is
// derived in the shell and never becomes a file path, so this is a value check,
// not a path-traversal defence.
var sessPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// AddUsage documents the add subcommand.
const AddUsage = "add (--cmd <string|-> | --cmd-env <name>) [--cwd <path>] [--exit <int>] " +
	"[--dur <ms>] [--ts <unix-ms>] [--sess <id>] [--sh <name>]"

// Add records one history entry.
//
// It always returns 0. This is the only subcommand called from a shell hook,
// and a non-zero status there would surface as a broken prompt or, under
// "set -e", a dead shell. Failures are reported on stderr instead of being
// swallowed: a history tool that quietly records nothing is not discovered
// until the day someone goes looking for a command that was never saved.
func Add(env *Env, args []string) int {
	fs := newFlagSet(env, "add", AddUsage)
	cmd := fs.String("cmd", "", "command string, or - to read it from stdin")
	cmdEnv := fs.String("cmd-env", "", "name of an environment variable holding the command")
	cwd := fs.String("cwd", "", "working directory the command ran in")
	sess := fs.String("sess", "", "session id")
	sh := fs.String("sh", "", "shell name (zsh, bash, fish)")
	var exit, dur, ts optInt
	fs.Var(&exit, "exit", "exit status of the command")
	fs.Var(&dur, "dur", "duration in milliseconds")
	fs.Var(&ts, "ts", "start time in Unix milliseconds")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	if fs.NArg() > 0 {
		env.errf("add takes no positional arguments (did you mean --cmd?)")
		return 0
	}

	if *cmd != "" && *cmdEnv != "" {
		env.errf("--cmd and --cmd-env cannot both be given")
		return 0
	}

	line := *cmd
	switch {
	case *cmdEnv != "":
		// How the shell hooks pass the command. An argument would be readable
		// by every user on the host through /proc/<pid>/cmdline for as long as
		// endap runs, and `export TOKEN=...` is a shell builtin that spawned no
		// process at all before endap was installed. /proc/<pid>/environ is
		// readable only by the owner, which is as narrow as this can be made
		// without paying for another process on every prompt.
		//
		// The ignore list cannot help here: by the time endap can evaluate it,
		// the arguments have already been visible.
		v, ok := os.LookupEnv(*cmdEnv)
		if !ok {
			env.errf("--cmd-env %s: the variable is not set", *cmdEnv)
			return 0
		}
		line = v
	case line == "-":
		// The long-command path. Shell builtins accept command lines longer
		// than ARG_MAX, which the environment shares, so past a threshold the
		// hook switches to stdin rather than letting the exec of endap fail.
		//
		// Read verbatim: the hooks write the command with no trailing newline,
		// so trimming one would silently drop a newline the user actually typed.
		b, err := io.ReadAll(env.Stdin)
		if err != nil {
			env.errf("cannot read the command from stdin: %v", err)
			return 0
		}
		line = string(b)
	}

	cfg := loadConfig(env)
	for _, w := range cfg.Critical {
		env.errf("%s", w)
	}
	if !Recordable(cfg, line) {
		return 0
	}

	now := time.Now().UnixMilli()
	start := now
	if ts.set {
		// add runs after the command has finished, so the start time has to come
		// from the hook. Working it out as "now minus dur" would corrupt ts on
		// every shell that cannot measure dur.
		start = ts.value
	}
	rec := record.Record{
		V:    record.Version,
		ID:   record.NewID(start),
		TS:   start,
		Cmd:  line,
		Cwd:  *cwd,
		Host: config.Host(),
		Sh:   *sh,
	}
	if s := *sess; s != "" {
		if sessPattern.MatchString(s) {
			rec.Sess = s
		} else {
			env.errf("ignoring malformed session id %q", s)
		}
	}
	if dur.set && dur.value >= 0 {
		rec.SetDur(dur.value)
	}
	if exit.set {
		rec.SetExit(int(exit.value))
	}

	data, err := record.Encode(&rec)
	if err != nil {
		env.errf("cannot encode the record: %v", err)
		return 0
	}
	path, err := historyPath()
	if err != nil {
		env.errf("%v", err)
		return 0
	}
	if err := store.Append(path, data); err != nil {
		env.errf("cannot write to %s: %v", path, err)
	}
	return 0
}

// Recordable reports whether a command string should be written to the log.
//
// The leading-space rule follows the HIST_IGNORE_SPACE convention, and is the
// reason the bash integration takes the command text from "history 1" rather
// than from $BASH_COMMAND, which strips it.
func Recordable(cfg *config.Config, cmd string) bool {
	if strings.TrimSpace(cmd) == "" {
		return false
	}
	if cmd[0] == ' ' || cmd[0] == '\t' {
		return false
	}
	return !cfg.Ignore.Match(cmd)
}
