package shell_test

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The Ctrl-R widget needs two things a plain pipe cannot give it.
//
// It needs a terminal: zsh turns its line editor off when there is none, and
// `zle -N` and `bindkey` then both return 0 without the widget ever running --
// a test built that way goes green having exercised nothing.
//
// And it needs the keys to arrive while the shell is sitting in the line
// editor. Ctrl-R is the tty's own "reprint" character, so anything typed while
// the shell is busy is swallowed by the line discipline before zsh sees it.
// Hence script(1) for the pty, and a pause between keystrokes.
func typeIntoZsh(t *testing.T, zdotdir string, keys []string) {
	t.Helper()
	scriptBin := ptyHarness(t)
	// -d skips the global rc files. Ubuntu's /etc/zsh/zshrc runs compinit, and
	// where $fpath holds a group-writable directory -- it does on the GitHub
	// runners -- compinit stops to ask "Ignore insecure directories and
	// continue [y] or abort compinit [n]?". That read happens before the line
	// editor is up and swallows the first key the test types, so the typed text
	// arrives one character short. What is under test is endap's own init
	// snippet, not the distribution's.
	cmd := exec.Command(scriptBin, "-qec", "zsh -d -i", "/dev/null")
	cmd.Dir = zdotdir
	cmd.Env = append(os.Environ(), "ZDOTDIR="+zdotdir, "HOME="+zdotdir, "TERM=xterm")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	// script(1) writes the session to its own stdout. Capturing it through a
	// real file rather than a pipe keeps everything the shell printed available
	// when a failure has to be explained.
	logFile, err := os.CreateTemp(zdotdir, "shell-output-*.log")
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
		// Wait for the shell to actually reach its first prompt rather than
		// guessing at a delay. Ctrl-R is the tty's reprint character, so a key
		// sent while the shell is still starting is eaten by the line
		// discipline and the test fails for a reason that has nothing to do
		// with the code under test.
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
		t.Fatalf("the shell did not exit:\n%s", shellOutput(logFile.Name()))
	}
	<-done
	t.Logf("shell output:\n%s", shellOutput(logFile.Name()))
}

// ptyHarness returns the path to script(1), or skips.
//
// The BSD script(1) on macOS takes its arguments in a different order, so the
// -qec form here is Linux-only. Skipping says so instead of failing with an
// error about a missing file.
func ptyHarness(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("the pty harness uses util-linux script(1); %s has the BSD one", runtime.GOOS)
	}
	scriptBin, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script(1) is not available to allocate a pty")
	}
	return scriptBin
}

// waitForOutput blocks until the file has content or the deadline passes.
func waitForOutput(path string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			// Give the prompt a moment to finish being drawn.
			time.Sleep(200 * time.Millisecond)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func shellOutput(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(could not read the shell output: " + err.Error() + ")"
	}
	return string(b)
}

// setupWidget builds a zsh that has endap installed and a stub standing in for
// fzf, so the selection is fixed and no one has to press anything.
func setupWidget(t *testing.T, selection string) (zdotdir, dataDir string) {
	t.Helper()
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh is not installed")
	}
	bin := endap(t)
	dir := t.TempDir()
	dataDir = filepath.Join(dir, "data")
	stubDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(stubDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The stub reads the candidate list the way fzf does and answers with a
	// fixed choice, NUL-terminated as --print0 would.
	stub := "#!/bin/sh\ncat > /dev/null\nprintf '%s\\0' " + shellQuote(selection) + "\n"
	if err := os.WriteFile(filepath.Join(stubDir, "fzf"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	rc := fmt.Sprintf(`export ENDAP_DATA_DIR=%q
export ENDAP_CONFIG=%q
export ENDAP_HOST=testhost
export PATH=%q:%q:$PATH
PS1='%%%% '
eval "$(endap init zsh)"
`, dataDir, filepath.Join(dir, "config"), stubDir, filepath.Dir(bin))
	if err := os.WriteFile(filepath.Join(dir, ".zshrc"), []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, dataDir
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func readLog(t *testing.T, dataDir string) []rec {
	t.Helper()
	var recs []rec
	data, err := os.ReadFile(filepath.Join(dataDir, "history.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var x rec
		if err := json.Unmarshal([]byte(line), &x); err != nil {
			t.Fatalf("not a record: %q", line)
		}
		recs = append(recs, x)
	}
	return recs
}

// TestCtrlRWidget drives the whole Ctrl-R path: the key reaches the widget, the
// widget runs the pipeline, strips the trailing NUL, puts the result in BUFFER,
// and Enter runs it. The proof is indirect and all the stronger for it -- the
// selected command shows up in the log, which can only happen if the buffer was
// filled correctly.
func TestCtrlRWidget(t *testing.T) {
	if testing.Short() {
		t.Skip("this test drives a pty and takes a few seconds")
	}
	// A multi-line selection, which is the case the whole JSON log format
	// exists for. If the NUL handling or the buffer assignment were wrong, this
	// is where it would show.
	const selection = "echo picked-one\necho picked-two"
	zdotdir, dataDir := setupWidget(t, selection)
	typeIntoZsh(t, zdotdir, []string{"echo seed\n", "\x12", "\n", "exit\n"})

	recs := readLog(t, dataDir)
	var got []string
	for _, r := range recs {
		got = append(got, r.Cmd)
	}
	if len(recs) < 2 {
		t.Fatalf("Ctrl-R did not put anything on the command line; recorded %q", got)
	}
	found := false
	for _, r := range recs {
		if r.Cmd == selection {
			found = true
		}
		if strings.HasSuffix(r.Cmd, "\x00") {
			t.Errorf("the trailing NUL was not stripped from %q", r.Cmd)
		}
	}
	if !found {
		t.Fatalf("the selection did not survive Ctrl-R intact; recorded %q", got)
	}
}

// TestCtrlRWidgetUsesTheTypedTextAsTheQuery: whatever is already on the line
// becomes fzf's initial query, which is what fzf's and atuin's own bindings do.
func TestCtrlRWidgetUsesTheTypedTextAsTheQuery(t *testing.T) {
	if testing.Short() {
		t.Skip("this test drives a pty and takes a few seconds")
	}
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh is not installed")
	}
	bin := endap(t)
	dir := t.TempDir()
	stubDir := filepath.Join(dir, "bin")
	queryFile := filepath.Join(dir, "query")
	if err := os.MkdirAll(stubDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Record whatever --query the widget passed, then answer with something
	// short so the shell has a command to run.
	stub := "#!/bin/sh\ncat > /dev/null\nfor a in \"$@\"; do case \"$a\" in --query=*) printf '%s' \"${a#--query=}\" > " +
		queryFile + ";; esac; done\nprintf 'true\\0'\n"
	if err := os.WriteFile(filepath.Join(stubDir, "fzf"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	rc := fmt.Sprintf("export ENDAP_DATA_DIR=%q\nexport ENDAP_CONFIG=%q\nexport PATH=%q:%q:$PATH\nPS1='%%%% '\neval \"$(endap init zsh)\"\n",
		filepath.Join(dir, "data"), filepath.Join(dir, "config"), stubDir, filepath.Dir(bin))
	if err := os.WriteFile(filepath.Join(dir, ".zshrc"), []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
	typeIntoZsh(t, dir, []string{"git sta", "\x12", "\n", "exit\n"})

	got, err := os.ReadFile(queryFile)
	if err != nil {
		t.Fatalf("the widget never ran fzf: %v", err)
	}
	if string(got) != "git sta" {
		t.Fatalf("--query was %q, want %q", got, "git sta")
	}
}

// TestBashCtrlRWidget is the same path in bash, where the pieces are different
// enough to be worth their own check: bind -x instead of zle, READLINE_LINE
// instead of BUFFER, and a process substitution instead of a command
// substitution -- bash discards NUL bytes from "$(...)" and prints a warning
// about it every time.
func TestBashCtrlRWidget(t *testing.T) {
	if testing.Short() {
		t.Skip("this test drives a pty and takes a few seconds")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	bin := endap(t)
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	stubDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(stubDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const selection = "echo picked-one"
	stub := "#!/bin/sh\ncat > /dev/null\nprintf '%s\\0' " + shellQuote(selection) + "\n"
	if err := os.WriteFile(filepath.Join(stubDir, "fzf"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	rc := fmt.Sprintf(`export ENDAP_DATA_DIR=%q
export ENDAP_CONFIG=%q
export PATH=%q:%q:$PATH
HISTFILE=%q
HISTCONTROL=ignorespace
PS1='$ '
eval "$(endap init bash)"
`, dataDir, filepath.Join(dir, "config"), stubDir, filepath.Dir(bin), filepath.Join(dir, "bash_history"))
	rcPath := filepath.Join(dir, "bashrc")
	if err := os.WriteFile(rcPath, []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionLog := typeIntoBash(t, dir, rcPath, []string{"echo seed\n", "\x12", "\n", "exit\n"})

	var got []string
	for _, r := range readLog(t, dataDir) {
		got = append(got, r.Cmd)
		if strings.HasSuffix(r.Cmd, "\x00") {
			t.Errorf("the trailing NUL was not stripped from %q", r.Cmd)
		}
	}
	found := false
	for _, c := range got {
		if c == selection {
			found = true
		}
	}
	if !found {
		t.Fatalf("Ctrl-R did not put the selection on the command line; recorded %q", got)
	}
	// The widget reads fzf's NUL-terminated output through a process
	// substitution rather than "$(...)", because bash discards NUL bytes from
	// command substitution and says so on stderr every single time. A
	// regression to "$(...)" still round-trips a single-line selection, so the
	// warning is what gives it away.
	if out := shellOutput(sessionLog); strings.Contains(out, "ignored null byte") {
		t.Fatalf("bash warned about a null byte, so the widget is using command substitution:\n%s", out)
	}
}

// TestBashCtrlRWidgetUsesTheTypedTextAsTheQuery is the bash half of the query
// behaviour. It is not the same code as zsh's: bash has no LBUFFER, so the
// widget slices READLINE_LINE at READLINE_POINT by hand, and an off-by-one
// there would go unnoticed with only the zsh test.
func TestBashCtrlRWidgetUsesTheTypedTextAsTheQuery(t *testing.T) {
	if testing.Short() {
		t.Skip("this test drives a pty and takes a few seconds")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	bin := endap(t)
	dir := t.TempDir()
	stubDir := filepath.Join(dir, "bin")
	queryFile := filepath.Join(dir, "query")
	if err := os.MkdirAll(stubDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\ncat > /dev/null\nfor a in \"$@\"; do case \"$a\" in --query=*) printf '%s' \"${a#--query=}\" > " +
		queryFile + ";; esac; done\nprintf 'true\\0'\n"
	if err := os.WriteFile(filepath.Join(stubDir, "fzf"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	rc := fmt.Sprintf(`export ENDAP_DATA_DIR=%q
export ENDAP_CONFIG=%q
export PATH=%q:%q:$PATH
HISTFILE=%q
HISTCONTROL=ignorespace
PS1='$ '
eval "$(endap init bash)"
`, filepath.Join(dir, "data"), filepath.Join(dir, "config"), stubDir, filepath.Dir(bin), filepath.Join(dir, "bash_history"))
	rcPath := filepath.Join(dir, "bashrc")
	if err := os.WriteFile(rcPath, []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
	typeIntoBash(t, dir, rcPath, []string{"git sta", "\x12", "\n", "exit\n"})

	got, err := os.ReadFile(queryFile)
	if err != nil {
		t.Fatalf("the widget never ran fzf: %v", err)
	}
	if string(got) != "git sta" {
		t.Fatalf("--query was %q, want %q", got, "git sta")
	}
}

// typeIntoBash drives bash and returns the path of the captured session log.
func typeIntoBash(t *testing.T, dir, rcPath string, keys []string) string {
	t.Helper()
	scriptBin := ptyHarness(t)
	cmd := exec.Command(scriptBin, "-qec", "bash --rcfile "+rcPath+" -i", "/dev/null")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir, "TERM=xterm")
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
		t.Fatalf("the shell did not exit:\n%s", shellOutput(logFile.Name()))
	}
	<-done
	t.Logf("shell output:\n%s", shellOutput(logFile.Name()))
	return logFile.Name()
}

// TestZshPrecmdKeepsTheExitStatus: zsh saves and restores $? around each precmd
// hook, so endap's hook cannot hide a failure from the ones after it. bash does
// not, which is why the bash integration has to restore it by hand.
func TestZshPrecmdKeepsTheExitStatus(t *testing.T) {
	r := runShell(t, "zsh", "__seen() { print -r -- \"SEEN=$?\" }\nautoload -Uz add-zsh-hook\nadd-zsh-hook precmd __seen",
		"false\necho done\n")
	if !strings.Contains(r.out, "SEEN=1") {
		t.Fatalf("a later precmd hook did not see exit status 1:\n%s", r.out)
	}
}
