// Package cli implements the endap subcommands.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/shirou/endap/internal/config"
	"github.com/shirou/endap/internal/store"
)

// Env carries the process environment a subcommand runs in, so tests can drive
// a subcommand without touching the real stdio.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// OSEnv returns the environment backed by the real process streams.
func OSEnv() *Env {
	return &Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
}

func (e *Env) errf(format string, args ...any) {
	fmt.Fprintf(e.Stderr, "endap: "+format+"\n", args...)
}

// optInt is an integer flag that remembers whether it was given at all, which
// matters for --exit: 0 is the most common real value, so "absent" cannot be
// spelled as zero.
type optInt struct {
	value int64
	set   bool
}

func (o *optInt) String() string {
	if o == nil || !o.set {
		return ""
	}
	return strconv.FormatInt(o.value, 10)
}

func (o *optInt) Set(s string) error {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	o.value, o.set = v, true
	return nil
}

// newFlagSet returns a FlagSet that reports errors through env and never exits
// the process on its own.
func newFlagSet(env *Env, name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "usage: endap %s\n", usage)
		fs.PrintDefaults()
	}
	return fs
}

// parseArgs parses flags that may appear after positional arguments.
//
// flag.Parse stops at the first non-flag word, which would make
// "endap import bash --file x" treat --file as a second positional. The spec
// writes the commands that way, and so would anyone reaching for them.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// rejectPositional reports a leftover word, for the subcommands that take no
// positional arguments at all.
//
// flag.Parse stops at the first non-flag word and hands the rest back, so a
// subcommand that never reads fs.Args() accepts "endap list --print0 false" and
// then does the opposite of what it was asked: --print0 is a boolean and stays
// on, the stray word is dropped, and every flag written after it is never
// parsed. Silently is the wrong way to get that wrong.
func rejectPositional(env *Env, fs *flag.FlagSet, args []string) bool {
	rest := fs.Args()
	if len(rest) == 0 {
		return false
	}
	// Parse consumed everything ahead of rest, so the token in front of the
	// stray word is the flag it was meant for. Naming it is most of the help:
	// a boolean flag is the only kind that can be written this way and still
	// parse.
	if i := len(args) - len(rest) - 1; i > -1 && args[i] != "--" && strings.HasPrefix(args[i], "-") {
		if _, err := strconv.ParseBool(rest[0]); err == nil {
			env.errf("unexpected argument %q; write %s=%s, a boolean flag takes no separate value", rest[0], args[i], rest[0])
			return true
		}
	}
	env.errf("unexpected argument %q", rest[0])
	return true
}

// loadConfig reads the config file, or returns the defaults when it cannot.
//
// A broken config must never stop a command from running: recording history
// matters more than honouring a setting, and the failure is reported instead of
// swallowed.
func loadConfig(env *Env) *config.Config {
	path, err := store.ConfigPath()
	if err != nil {
		env.errf("cannot locate the config file: %v", err)
		return config.Defaults()
	}
	cfg, err := config.Load(path)
	if err != nil {
		env.errf("%v (continuing with defaults)", err)
		return config.Defaults()
	}
	return cfg
}

func historyPath() (string, error) {
	p, err := store.HistoryPath()
	if err != nil {
		return "", fmt.Errorf("cannot locate the data directory: %w", err)
	}
	return p, nil
}
