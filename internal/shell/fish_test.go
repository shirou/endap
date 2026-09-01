package shell_test

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fish is driven through a pty because its preexec and postexec events come
// from the interactive reader. Feed it commands on a pipe and the events never
// fire at all -- the commands run, and nothing is recorded. That is the same
// shape of problem as zsh's line editor being absent under a pipe, and it is
// why this file uses script(1) rather than an exec.Cmd with a Stdin reader.
func runFish(t *testing.T, input string) []rec {
	t.Helper()
	if _, err := exec.LookPath("fish"); err != nil {
		t.Skip("fish is not installed")
	}
	scriptBin, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script(1) is not available to allocate a pty")
	}
	bin := endap(t)

	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	dataDir := filepath.Join(dir, "data")
	for _, d := range []string{home, filepath.Join(dir, "xdg-data"), filepath.Join(dir, "xdg-config")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	initFile := filepath.Join(dir, "init.fish")
	script, err := exec.Command(bin, "init", "fish").Output()
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("set -gx ENDAP_DATA_DIR %q\nset -gx ENDAP_CONFIG %q\nset -gx ENDAP_HOST testhost\n%s",
		dataDir, filepath.Join(dir, "config"), script)
	if err := os.WriteFile(initFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(scriptBin, "-qec", "fish -i -C 'source "+initFile+"'", "/dev/null")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)
	// fish writes a default config and imports other shells' history on first
	// run. Every path it could use is redirected into the test directory so it
	// cannot touch the real home.
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_DATA_HOME="+filepath.Join(dir, "xdg-data"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "xdg-config"),
		"XDG_STATE_HOME="+filepath.Join(dir, "xdg-state"),
		"HISTFILE="+filepath.Join(dir, "histfile"),
		"TERM=dumb",
		"PATH="+filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, _ := cmd.CombinedOutput()

	var recs []rec
	data, err := os.ReadFile(filepath.Join(dataDir, "history.jsonl"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading the log: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var x rec
		if err := json.Unmarshal([]byte(line), &x); err != nil {
			t.Fatalf("the shell wrote a line that is not a record: %q", line)
		}
		recs = append(recs, x)
	}
	return recs
}

func TestFishHooks(t *testing.T) {
	recs := runFish(t, "echo first-command\n echo leading-space\nsleep 0.4\nfalse\necho a | tr a b\n\n\nexit\n")
	got := map[string]*rec{}
	var order []string
	for i := range recs {
		got[recs[i].Cmd] = &recs[i]
		order = append(order, recs[i].Cmd)
	}
	if len(recs) == 0 {
		t.Fatal("nothing was recorded")
	}
	for _, c := range order {
		if strings.HasPrefix(c, " ") {
			t.Errorf("a command starting with a space was recorded: %q", c)
		}
	}
	if _, ok := got["echo first-command"]; !ok {
		t.Fatalf("recorded %q", order)
	}
	// fish_postexec does not fire for an empty line, so no guard is needed on
	// the fish side and no ghost record appears.
	n := 0
	for _, c := range order {
		if c == "echo first-command" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("empty lines produced %d copies of the first command", n)
	}
	s, ok := got["sleep 0.4"]
	if !ok {
		t.Fatalf("sleep was not recorded; recorded %q", order)
	}
	if s.Dur == nil || *s.Dur < 300 || *s.Dur > 900 {
		t.Errorf("dur = %v, want roughly 400ms from $CMD_DURATION", s.Dur)
	}
	if s.TS <= 0 {
		t.Errorf("ts = %d", s.TS)
	}
	f, ok := got["false"]
	if !ok || f.Exit == nil || *f.Exit != 1 {
		t.Errorf("exit = %v, want 1", f)
	}
	if _, ok := got["echo a | tr a b"]; !ok {
		t.Errorf("the pipeline was not recorded as one command: %q", order)
	}
	for _, r := range recs {
		if r.Sh != "fish" {
			t.Errorf("sh = %q", r.Sh)
		}
		if r.Sess == "" {
			t.Error("no session id")
		}
	}
}

func TestFishScriptParses(t *testing.T) {
	if _, err := exec.LookPath("fish"); err != nil {
		t.Skip("fish is not installed")
	}
	bin := endap(t)
	for _, opts := range [][]string{{}, {"--preview"}, {"--no-fzf-sort"}, {"--no-bindkey"}} {
		t.Run(strings.Join(append([]string{"fish"}, opts...), " "), func(t *testing.T) {
			script, err := exec.Command(bin, append([]string{"init", "fish"}, opts...)...).Output()
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "init.fish")
			if err := os.WriteFile(path, script, 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("fish", "--no-execute", path).CombinedOutput()
			if err != nil {
				t.Fatalf("fish --no-execute failed: %v\n%s\n--- script ---\n%s", err, out, script)
			}
		})
	}
}

// setupFishWidget builds a fish that has endap installed and a stub standing in
// for fzf, with everything fish writes redirected into the test directory.
func setupFishWidget(t *testing.T, stub string) (dir string) {
	t.Helper()
	if _, err := exec.LookPath("fish"); err != nil {
		t.Skip("fish is not installed")
	}
	bin := endap(t)
	dir = t.TempDir()
	for _, d := range []string{"home", "xdg-data", "xdg-config", "bin", "data"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "fzf"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	script, err := exec.Command(bin, "init", "fish").Output()
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("set -gx ENDAP_DATA_DIR %q\nset -gx ENDAP_CONFIG %q\nset -gx PATH %q %q $PATH\n%s",
		filepath.Join(dir, "data"), filepath.Join(dir, "config"),
		filepath.Join(dir, "bin"), filepath.Dir(bin), script)
	if err := os.WriteFile(filepath.Join(dir, "init.fish"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// typeIntoFish drives an interactive fish through a pty and returns the session
// log. fish only emits its events, and only runs key bindings, from the
// interactive reader, so a pipe would exercise neither.
func typeIntoFish(t *testing.T, dir string, keys []string) string {
	t.Helper()
	scriptBin := ptyHarness(t)
	cmd := exec.Command(scriptBin, "-qec",
		"fish -i -C 'source "+filepath.Join(dir, "init.fish")+"'", "/dev/null")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HOME="+filepath.Join(dir, "home"),
		"XDG_DATA_HOME="+filepath.Join(dir, "xdg-data"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "xdg-config"),
		"XDG_STATE_HOME="+filepath.Join(dir, "xdg-state"),
		"HISTFILE="+filepath.Join(dir, "histfile"),
		"TERM=xterm",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := os.CreateTemp(dir, "shell-output-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		waitForOutput(logFile.Name(), 15*time.Second)
		for _, k := range keys {
			io.WriteString(stdin, k)
			time.Sleep(500 * time.Millisecond)
		}
		stdin.Close()
	}()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case <-waited:
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		<-waited
		t.Fatalf("fish did not exit:\n%s", shellOutput(logFile.Name()))
	}
	<-done
	t.Logf("shell output:\n%s", shellOutput(logFile.Name()))
	return logFile.Name()
}

// TestFishCtrlRWidget drives the whole Ctrl-R path in fish: the binding fires,
// the NUL-separated selection comes back through `string split0` with its
// newlines intact, and the result reaches the command line.
func TestFishCtrlRWidget(t *testing.T) {
	if testing.Short() {
		t.Skip("this test drives a pty and takes a few seconds")
	}
	// The blank line in the middle is the point. fish splits an ordinary command
	// substitution on newlines and drops the empty pieces, so a selection that
	// is merely two lines survives either way; only a run of newlines tells the
	// two apart, and `string split0` is what makes fish split on NUL instead.
	const selection = "echo picked-one\n\necho picked-two"
	stub := "#!/bin/sh\ncat > /dev/null\nprintf '%s\\0' " + shellQuote(selection) + "\n"
	dir := setupFishWidget(t, stub)
	typeIntoFish(t, dir, []string{"echo seed\n", "\x12", "\n", "exit\n"})

	var got []string
	data, err := os.ReadFile(filepath.Join(dir, "data", "history.jsonl"))
	if err != nil {
		t.Fatalf("nothing was recorded: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var x rec
		if err := json.Unmarshal([]byte(line), &x); err != nil {
			t.Fatalf("not a record: %q", line)
		}
		got = append(got, x.Cmd)
	}
	found := false
	for _, c := range got {
		if c == selection {
			found = true
		}
		if strings.HasSuffix(c, "\x00") {
			t.Errorf("the trailing NUL was not stripped from %q", c)
		}
	}
	if !found {
		t.Fatalf("Ctrl-R did not put the multi-line selection on the command line; recorded %q", got)
	}
}

// TestFishCtrlRWidgetQueryStopsAtTheCursor: fish's `commandline -b` returns the
// whole line, so the widget has to ask for `-c` as well to match what zsh's
// LBUFFER and bash's READLINE_POINT slice give fzf.
func TestFishCtrlRWidgetQueryStopsAtTheCursor(t *testing.T) {
	if testing.Short() {
		t.Skip("this test drives a pty and takes a few seconds")
	}
	stub := "#!/bin/sh\ncat > /dev/null\nfor a in \"$@\"; do case \"$a\" in --query=*) printf '%s' \"${a#--query=}\" > \"$ENDAP_TEST_QUERY_FILE\";; esac; done\nprintf 'true\\0'\n"
	dir := setupFishWidget(t, stub)
	queryFile := filepath.Join(dir, "query")
	body, err := os.ReadFile(filepath.Join(dir, "init.fish"))
	if err != nil {
		t.Fatal(err)
	}
	body = append([]byte(fmt.Sprintf("set -gx ENDAP_TEST_QUERY_FILE %q\n", queryFile)), body...)
	if err := os.WriteFile(filepath.Join(dir, "init.fish"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	// Type "abcdef", walk the cursor back three, then Ctrl-R. fzf should be
	// asked for "abc", not "abcdef".
	typeIntoFish(t, dir, []string{"abcdef", "\x1b[D\x1b[D\x1b[D", "\x12", "\n", "exit\n"})

	got, err := os.ReadFile(queryFile)
	if err != nil {
		t.Fatalf("the widget never ran fzf: %v", err)
	}
	if string(got) != "abc" {
		t.Fatalf("--query was %q, want %q (the whole line would be \"abcdef\")", got, "abc")
	}
}
