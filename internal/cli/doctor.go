package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/endap/internal/store"
)

// DoctorUsage documents the doctor subcommand.
const DoctorUsage = "doctor [--compact-threshold <n>]"

// fzf version requirements (D10). The options were checked against fzf's
// changelog: --no-sort / --read0 / --print0 predate 0.33, --scheme=history
// arrived in 0.33.0, multi-line display of --read0 input in 0.53.0, and
// --accept-nth in 0.60.0.
const (
	fzfRequired    = "0.33"
	fzfRecommended = "0.53"
	fzfPreview     = "0.60"
)

type finding struct {
	level string // "ok", "warn", "error"
	text  string
}

// Doctor inspects the installation and the log.
//
// It exits non-zero when it finds an error, which is what makes it usable as a
// check rather than as decoration. Only `add` is required to always succeed.
func Doctor(env *Env, args []string) int {
	fs := newFlagSet(env, "doctor", DoctorUsage)
	threshold := fs.Int("compact-threshold", 200000, "record count above which compaction is suggested")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var out []finding
	add := func(level, format string, a ...any) {
		out = append(out, finding{level, fmt.Sprintf(format, a...)})
	}

	// --- data directory ---
	path, err := historyPath()
	if err != nil {
		add("error", "%v", err)
		return report(env, out)
	}
	dir := filepath.Dir(path)
	add("ok", "data directory: %s", dir)
	checkDir(dir, add)
	checkFilePerm(path, add)
	checkFilePerm(store.RejPath(path), add)

	// --- config ---
	cfg := loadConfig(env)
	if cfg.Path == "" {
		add("ok", "config: none (using defaults)")
	} else {
		add("ok", "config: %s", cfg.Path)
	}
	for _, w := range cfg.Warnings {
		add("warn", "config: %s", w)
	}
	for _, w := range cfg.Critical {
		add("error", "config: %s", w)
	}
	add("ok", "halflife %s, cwd_boost %g, fail_penalty %g", cfg.Halflife, cfg.CwdBoost, cfg.FailPenalty)
	add("ok", "active ignore patterns (%d):", cfg.Ignore.Len())
	for _, p := range cfg.Ignore.Sources() {
		add("ok", "    %s", p)
	}

	// --- the log itself ---
	st, err := store.WalkFile(path, nil)
	if err != nil {
		add("error", "cannot read %s: %v", path, err)
	} else {
		add("ok", "%d records (%d lines)", st.Records, st.Lines)
		if st.Corrupt > 0 {
			add("error", "%d corrupt lines in %s; `endap compact` moves them to %s",
				st.Corrupt, filepath.Base(path), store.RejName)
		}
		if st.Unknown > 0 {
			add("warn", "%d records from a newer endap; they are kept but not ranked", st.Unknown)
		}
		if st.Records > *threshold {
			add("warn", "%d records is past the %d threshold; consider `endap compact`", st.Records, *threshold)
		}
	}
	if info, err := os.Stat(store.RejPath(path)); err == nil {
		n, _ := countLines(store.RejPath(path))
		add("warn", "%s holds %d rejected lines (%d bytes)", store.RejName, n, info.Size())
	}

	// --- leftovers from an interrupted rewrite ---
	if tmps, err := store.TmpFiles(path); err == nil {
		for _, t := range tmps {
			add("warn", "leftover rewrite temporary %s; a compact or forget did not finish", filepath.Base(t))
		}
	}
	lock := store.LockPath(path)
	if info, err := os.Stat(lock); err == nil {
		add("warn", "stale lock %s from %s; remove it if no rewrite is running",
			filepath.Base(lock), info.ModTime().Format(time.RFC3339))
	}

	// --- fzf ---
	checkFzf(add)

	// --- bash ---
	checkBash(add)

	return report(env, out)
}

func report(env *Env, out []finding) int {
	code := 0
	for _, f := range out {
		switch f.level {
		case "error":
			code = 1
			fmt.Fprintf(env.Stdout, "ERROR  %s\n", f.text)
		case "warn":
			fmt.Fprintf(env.Stdout, "WARN   %s\n", f.text)
		default:
			fmt.Fprintf(env.Stdout, "ok     %s\n", f.text)
		}
	}
	return code
}

func checkDir(dir string, add func(string, string, ...any)) {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			add("warn", "the data directory does not exist yet; it is created on the first `endap add`")
			return
		}
		add("error", "cannot stat %s: %v", dir, err)
		return
	}
	if perm := info.Mode().Perm(); perm != store.DirPerm {
		add("warn", "%s is %#o, expected %#o", dir, perm, store.DirPerm)
	}
	probe := filepath.Join(dir, ".endap-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, store.FilePerm)
	if err != nil {
		add("error", "the data directory is not writable: %v", err)
		return
	}
	f.Close()
	os.Remove(probe)
}

func checkFilePerm(path string, add func(string, string, ...any)) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if perm := info.Mode().Perm(); perm != store.FilePerm {
		add("warn", "%s is %#o, expected %#o", filepath.Base(path), perm, store.FilePerm)
	}
}

func countLines(path string) (int, error) {
	data, err := store.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return len(store.SplitLines(data)), nil
}

var fzfVersionRe = regexp.MustCompile(`([0-9]+)\.([0-9]+)\.?([0-9]*)`)

func checkFzf(add func(string, string, ...any)) {
	bin, err := exec.LookPath("fzf")
	if err != nil {
		add("error", "fzf is not on PATH; Ctrl-R needs fzf %s or newer", fzfRequired)
		return
	}
	outBytes, err := exec.Command(bin, "--version").Output()
	if err != nil {
		add("warn", "could not run `fzf --version`: %v", err)
		return
	}
	version := strings.TrimSpace(string(outBytes))
	m := fzfVersionRe.FindStringSubmatch(version)
	if m == nil {
		add("warn", "could not read the fzf version from %q", version)
		return
	}
	got := [2]int{atoi(m[1]), atoi(m[2])}
	switch {
	case less(got, parseMinor(fzfRequired)):
		add("error", "fzf %s is older than the required %s", version, fzfRequired)
	case less(got, parseMinor(fzfRecommended)):
		add("warn", "fzf %s is older than %s, so multi-line commands are shown squashed onto one line", version, fzfRecommended)
	case less(got, parseMinor(fzfPreview)):
		add("warn", "fzf %s is older than %s; do not use `endap init <shell> --preview`, it would make fzf exit with an unknown-option error and take Ctrl-R with it", version, fzfPreview)
	default:
		add("ok", "fzf %s", version)
	}
}

func parseMinor(s string) [2]int {
	parts := strings.SplitN(s, ".", 3)
	v := [2]int{}
	if len(parts) > 0 {
		v[0] = atoi(parts[0])
	}
	if len(parts) > 1 {
		v[1] = atoi(parts[1])
	}
	return v
}

func less(a, b [2]int) bool {
	if a[0] != b[0] {
		return a[0] < b[0]
	}
	return a[1] < b[1]
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// checkBash reports the bash settings that keep commands out of bash's own
// history, and therefore out of endap's. In bash the command text comes from
// "history 1", so anything bash does not store cannot be recovered.
//
// bash exports none of these variables, so a child process cannot read them.
// The integration script republishes them under ENDAP_BASH_* on every prompt,
// which is what makes this check possible at all. Without the integration
// installed there is nothing to check and the section is skipped.
func checkBash(add func(string, string, ...any)) {
	hc, installed := os.LookupEnv("ENDAP_BASH_HISTCONTROL")
	if !installed {
		if os.Getenv("ENDAP_DOCTOR_SHELL") != "bash" && os.Getenv("BASH_VERSION") == "" {
			return
		}
		hc = os.Getenv("HISTCONTROL")
	}
	bad := []string{}
	for _, part := range strings.Split(hc, ":") {
		if part == "ignoredups" || part == "erasedups" || part == "ignoreboth" {
			bad = append(bad, part)
		}
	}
	if len(bad) > 0 {
		add("error", "HISTCONTROL contains %s: bash does not add a repeated command to its history, so endap cannot record repeated runs. Remove it, or accept the loss.",
			strings.Join(bad, " and "))
	} else if hc != "" {
		add("ok", "HISTCONTROL=%s", hc)
	}
	if hi := firstSet("ENDAP_BASH_HISTIGNORE", "HISTIGNORE"); hi != "" {
		add("error", "HISTIGNORE=%s: matching commands never reach bash's history, so endap cannot record them either", hi)
	}
	if hs := firstSet("ENDAP_BASH_HISTSIZE", "HISTSIZE"); hs != "" {
		if n, err := strconv.Atoi(hs); err == nil && n == 0 {
			add("error", "HISTSIZE=0 disables bash history, so endap can record nothing")
		}
	}
	if os.Getenv("ENDAP_BASH_HISTORY") == "off" {
		add("error", "`set +o history` is in effect, so endap can record nothing")
	}
	if os.Getenv("ENDAP_BASH_PROMPT_COMMAND_OK") == "0" {
		add("error", "something else has been added to PROMPT_COMMAND outside endap's hooks. "+
			"__endap_pre has to run first to capture $? and disarm the DEBUG trap, and __endap_arm has to run last. "+
			"Move the `eval \"$(endap init bash)\"` line after whatever else sets PROMPT_COMMAND.")
	}
}

// firstSet returns the first of the named environment variables that is set.
func firstSet(names ...string) string {
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok {
			return v
		}
	}
	return ""
}
