package shell_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// This file is V1a: the hooks are driven in a real shell rather than reasoned
// about. It is not V1b. A shell driven through a pipe has no line editor, so
// `zle -N` and `bindkey` both return 0 without the widget ever running: the
// Ctrl-R experience -- BUFFER, CURSOR, --query="$LBUFFER", the NUL strip --
// cannot be covered here and is checked by hand instead.

type rec struct {
	Cmd  string `json:"cmd"`
	TS   int64  `json:"ts"`
	Dur  *int64 `json:"dur"`
	Exit *int   `json:"exit"`
	Cwd  string `json:"cwd"`
	Sess string `json:"sess"`
	Sh   string `json:"sh"`
	Host string `json:"host"`
	V    int    `json:"v"`
	ID   string `json:"id"`
}

type shellRun struct {
	t    *testing.T
	dir  string
	recs []rec
	out  string
}

// buildEndap compiles the binary once per test binary run.
var endapBin string

func endap(t *testing.T) string {
	t.Helper()
	if endapBin != "" {
		return endapBin
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	dir, err := os.MkdirTemp("", "endap-bin")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "endap")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/shirou/endap").CombinedOutput()
	if err != nil {
		t.Fatalf("building endap: %v\n%s", err, out)
	}
	endapBin = bin
	return bin
}

// runShell starts an interactive shell with its own dot files and feeds it
// input.
//
// ZDOTDIR and HOME are redirected on purpose. Without that the user's own
// ~/.zshrc is read, and its atuin bindings, fzf widgets and preexec hooks end
// up inside the test.
func runShell(t *testing.T, shell, rc, input string) *shellRun {
	t.Helper()
	bin := endap(t)
	if _, err := exec.LookPath(shell); err != nil {
		t.Skipf("%s is not installed", shell)
	}
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	preamble := fmt.Sprintf("export ENDAP_DATA_DIR=%q\nexport ENDAP_CONFIG=%q\nexport ENDAP_HOST=testhost\n",
		dataDir, filepath.Join(dir, "config"))

	var cmd *exec.Cmd
	switch shell {
	case "zsh":
		if err := os.WriteFile(filepath.Join(dir, ".zshrc"),
			[]byte(preamble+rc+"\neval \"$("+bin+" init zsh)\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd = exec.Command(shell, "-is")
	case "bash":
		rcPath := filepath.Join(dir, "bashrc")
		if err := os.WriteFile(rcPath,
			[]byte(preamble+"HISTFILE="+filepath.Join(dir, "bash_history")+"\n"+rc+
				"\neval \"$("+bin+" init bash)\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd = exec.Command(shell, "--rcfile", rcPath, "-i")
	default:
		t.Fatalf("unsupported shell %q", shell)
	}
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"ZDOTDIR="+dir, "HOME="+dir,
		"PATH="+filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ENDAP_DATA_DIR="+dataDir,
	)
	cmd.Stdin = strings.NewReader(input)
	out, _ := cmd.CombinedOutput()

	r := &shellRun{t: t, dir: dir, out: string(out)}
	data, err := os.ReadFile(filepath.Join(dataDir, "history.jsonl"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading the log: %v\nshell output:\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var x rec
		if err := json.Unmarshal([]byte(line), &x); err != nil {
			t.Fatalf("the shell wrote a line that is not a record: %q", line)
		}
		r.recs = append(r.recs, x)
	}
	return r
}

func (r *shellRun) cmds() []string {
	out := make([]string, len(r.recs))
	for i, x := range r.recs {
		out[i] = x.Cmd
	}
	return out
}

func (r *shellRun) find(cmd string) *rec {
	r.t.Helper()
	for i := range r.recs {
		if r.recs[i].Cmd == cmd {
			return &r.recs[i]
		}
	}
	r.t.Fatalf("no record for %q; recorded: %q\nshell output:\n%s", cmd, r.cmds(), r.out)
	return nil
}

func (r *shellRun) has(cmd string) bool {
	for _, x := range r.recs {
		if x.Cmd == cmd {
			return true
		}
	}
	return false
}

const hookScript = `echo first-command
 echo leading-space
sleep 0.4
false
echo a | tr a b


export API_KEY=deadbeefdeadbeef
echo 'multi
line'
echo redirect > out.log && echo ok
`

func TestHooks(t *testing.T) {
	for _, shell := range []string{"zsh", "bash"} {
		t.Run(shell, func(t *testing.T) {
			rc := ""
			if shell == "bash" {
				// Without this bash's default ignoreboth hides repeats, which
				// is a separate case covered by TestBashHistcontrol.
				rc = "HISTCONTROL=ignorespace"
			}
			r := runShell(t, shell, rc, hookScript)

			if !r.has("echo first-command") {
				t.Fatalf("nothing was recorded; shell output:\n%s", r.out)
			}
			// A leading space keeps a command out of the log entirely. In bash
			// this is why the command text comes from `history 1` and not from
			// $BASH_COMMAND, which strips it.
			for _, c := range r.cmds() {
				if strings.HasPrefix(c, " ") {
					t.Errorf("a command starting with a space was recorded: %q", c)
				}
			}
			if r.has("export API_KEY=deadbeefdeadbeef") {
				t.Error("a command matching the ignore list was recorded")
			}

			sleep := r.find("sleep 0.4")
			if sleep.Dur == nil {
				t.Fatal("no duration was recorded")
			}
			if *sleep.Dur < 300 || *sleep.Dur > 900 {
				t.Errorf("dur = %dms, want roughly 400", *sleep.Dur)
			}
			// ts is the start time, not the finish time.
			now := time.Now().UnixMilli()
			if sleep.TS <= 0 || sleep.TS > now {
				t.Errorf("ts = %d is not a start time (now %d)", sleep.TS, now)
			}
			if next := r.find("false"); next.TS-sleep.TS < 300 {
				t.Errorf("ts looks like the finish time: sleep at %d, next command at %d", sleep.TS, next.TS)
			}

			if f := r.find("false"); f.Exit == nil || *f.Exit != 1 {
				t.Errorf("exit = %v, want 1", f.Exit)
			}
			// A pipeline is one record, not one per simple command.
			if n := countCmd(r, "echo a | tr a b"); n != 1 {
				t.Errorf("the pipeline produced %d records, want 1", n)
			}
			if r.has("echo a") || r.has("tr a b") {
				t.Error("the pipeline was split into its parts")
			}

			// An empty line makes precmd fire with no preexec before it.
			// Without the guard the previous command is recorded again.
			if n := countCmd(r, "echo first-command"); n != 1 {
				t.Errorf("empty lines produced %d copies of the first command", n)
			}

			multi := r.find("echo 'multi\nline'")
			if !strings.Contains(multi.Cmd, "\n") {
				t.Error("the multi-line command lost its newline")
			}

			// > and && must survive: encoding/json escapes them by default.
			redirect := r.find("echo redirect > out.log && echo ok")
			if !strings.Contains(redirect.Cmd, "> out.log && echo") {
				t.Errorf("redirect mangled: %q", redirect.Cmd)
			}

			sess := r.recs[0].Sess
			if sess == "" {
				t.Error("no session id was recorded")
			}
			for _, x := range r.recs {
				if x.Sess != sess {
					t.Errorf("session id changed within one shell: %q then %q", sess, x.Sess)
				}
				if x.Sh != shell {
					t.Errorf("sh = %q, want %q", x.Sh, shell)
				}
				if x.Cwd == "" {
					t.Error("no working directory was recorded")
				}
			}
		})
	}
}

func countCmd(r *shellRun, cmd string) int {
	n := 0
	for _, x := range r.recs {
		if x.Cmd == cmd {
			n++
		}
	}
	return n
}

// TestCommandThatIsJustADash is a regression. The hooks used to pass short
// commands as an argument, so a command line of exactly "-" arrived as
// "endap add --cmd -" -- the spelling that means "read the command from
// stdin". stdin at a prompt is the terminal, so endap blocked, the prompt never
// came back, and the next thing typed was swallowed by endap and recorded as if
// it were the command that had just run.
func TestCommandThatIsJustADash(t *testing.T) {
	for _, shell := range []string{"zsh", "bash"} {
		t.Run(shell, func(t *testing.T) {
			rc := ""
			if shell == "bash" {
				rc = "HISTCONTROL=ignorespace"
			}
			r := runShell(t, shell, rc, "-\necho still alive\n")
			if !r.has("echo still alive") {
				t.Fatalf("the shell did not survive a command of \"-\"; recorded %q\n%s", r.cmds(), r.out)
			}
			if !r.has("-") {
				t.Errorf("the command \"-\" was not recorded; recorded %q", r.cmds())
			}
		})
	}
}

// TestCommandIsNotVisibleInArgv: the hooks pass the command through the
// environment because /proc/<pid>/cmdline is world-readable. A shell builtin
// like "export TOKEN=..." spawns no process at all without endap, so an
// argument here would be an exposure endap itself creates.
func TestCommandIsNotVisibleInArgv(t *testing.T) {
	for _, shell := range []string{"zsh", "bash"} {
		t.Run(shell, func(t *testing.T) {
			bin := endap(t)
			dir := t.TempDir()
			// A wrapper that records its own argv before handing over.
			wrapper := filepath.Join(dir, "bin")
			if err := os.MkdirAll(wrapper, 0o700); err != nil {
				t.Fatal(err)
			}
			argvLog := filepath.Join(dir, "argv.log")
			body := "#!/bin/sh\ntr '\\0' ' ' < /proc/$$/cmdline >> " + argvLog + "\necho >> " + argvLog + "\nexec " + bin + " \"$@\"\n"
			if err := os.WriteFile(filepath.Join(wrapper, "endap"), []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			rc := "export PATH=" + wrapper + ":$PATH"
			if shell == "bash" {
				rc += "\nHISTCONTROL=ignorespace"
			}
			const secret = "echo hunter2NOTASECRETREALLY"
			runShell(t, shell, rc, secret+"\n")

			argv, err := os.ReadFile(argvLog)
			if err != nil {
				t.Fatalf("the hook never ran endap: %v", err)
			}
			if strings.Contains(string(argv), "hunter2NOTASECRETREALLY") {
				t.Fatalf("the command is in argv, so every user on the host can read it:\n%s", argv)
			}
			if !strings.Contains(string(argv), "--cmd-env") {
				t.Fatalf("the hook did not use the environment path:\n%s", argv)
			}
		})
	}
}

func TestNoRecordOnAnEmptyShell(t *testing.T) {
	// The first prompt of a shell, and nothing but empty lines after it.
	for _, shell := range []string{"zsh", "bash"} {
		t.Run(shell, func(t *testing.T) {
			r := runShell(t, shell, "", "\n\n\n")
			if len(r.recs) != 0 {
				t.Fatalf("an idle shell recorded %q", r.cmds())
			}
		})
	}
}

func TestLongCommandGoesThroughStdin(t *testing.T) {
	// A shell builtin accepts a command line longer than execve will carry.
	// Passing it as an argument, or in the environment, would make the exec of
	// endap fail outright.
	//
	// The multi-byte case is the one the threshold has to be chosen for: Linux
	// caps a single argv or environment string at 128KiB, the shells count
	// characters, and 30000 characters of a four-byte rune is 120KB -- under
	// any character-counted threshold of 100000, and over the kernel's limit.
	cases := []struct {
		name string
		cmd  string
	}{
		{"ascii", ": " + strings.Repeat("x", 150000)},
		{"multibyte", ": " + strings.Repeat("\U0001F600", 30000)},
	}
	for _, shell := range []string{"zsh", "bash"} {
		for _, c := range cases {
			t.Run(shell+"/"+c.name, func(t *testing.T) {
				rc := ""
				if shell == "bash" {
					rc = "HISTCONTROL="
				}
				r := runShell(t, shell, rc, c.cmd+"\necho after\n")
				got := r.find(c.cmd)
				if got.Cmd != c.cmd {
					t.Fatalf("recorded %d bytes, want %d", len(got.Cmd), len(c.cmd))
				}
				if !r.has("echo after") {
					t.Fatal("the shell did not carry on after the long command")
				}
			})
		}
	}
}

// TestBashPromptCommand is the D9(2) and D9(3) regression: the DEBUG trap fires
// for the contents of PROMPT_COMMAND as well, and $? is whatever the hook
// before endap's left behind.
func TestBashPromptCommand(t *testing.T) {
	cases := []struct {
		name string
		rc   string
	}{
		{"empty", ""},
		{"single function", "__other() { return 42; }\nPROMPT_COMMAND=__other"},
		{"semicolons", `PROMPT_COMMAND="true; false; true"`},
		{"trailing semicolon", `PROMPT_COMMAND="true;"`},
		{"array", `PROMPT_COMMAND=( "true" "false" )`},
		{"exit 42 first", "__other() { return 42; }\nPROMPT_COMMAND=\"__other; true; __other\""},
		{"existing debug trap", `trap 'ENDAP_TEST_DBG=$((ENDAP_TEST_DBG+1))' DEBUG`},
		{"histtimeformat", `HISTTIMEFORMAT="%F-%T | "`},
		{"lithist", "shopt -s lithist"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runShell(t, "bash", "HISTCONTROL=ignorespace\n"+c.rc, "sleep 0.4\nfalse\necho done\n")
			s := r.find("sleep 0.4")
			if s.Dur == nil || *s.Dur < 300 || *s.Dur > 900 {
				t.Errorf("dur = %v, want roughly 400ms: PROMPT_COMMAND fired the DEBUG trap", s.Dur)
			}
			f := r.find("false")
			if f.Exit == nil || *f.Exit != 1 {
				t.Errorf("exit = %v, want 1: $? was taken from another PROMPT_COMMAND hook", f.Exit)
			}
			if !r.has("echo done") {
				t.Error("recording stopped after the first commands")
			}
		})
	}
}

// TestBashPreservesTheExitStatusForOtherHooks: endap runs first in
// PROMPT_COMMAND, so it must hand $? on unchanged to starship and friends.
func TestBashPreservesTheExitStatusForOtherHooks(t *testing.T) {
	rc := "HISTCONTROL=ignorespace\n__seen() { echo \"SEEN=$?\"; }\nPROMPT_COMMAND=__seen"
	r := runShell(t, "bash", rc, "false\necho done\n")
	if !strings.Contains(r.out, "SEEN=1") {
		t.Fatalf("the following hook did not see exit status 1:\n%s", r.out)
	}
}

// TestBashChainsAnExistingDebugTrap: replacing someone else's DEBUG trap would
// silently break whatever installed it.
func TestBashChainsAnExistingDebugTrap(t *testing.T) {
	rc := "HISTCONTROL=ignorespace\nENDAP_TEST_DBG=0\ntrap 'ENDAP_TEST_DBG=$((ENDAP_TEST_DBG+1))' DEBUG"
	r := runShell(t, "bash", rc, "echo one\necho \"DBG=$ENDAP_TEST_DBG\"\n")
	if strings.Contains(r.out, "DBG=0") {
		t.Fatalf("the existing DEBUG trap stopped firing:\n%s", r.out)
	}
	if !strings.Contains(r.out, "DBG=") {
		t.Fatalf("the test command did not run:\n%s", r.out)
	}
}

// TestBashChainedDebugTrapSeesTheExitStatus: chaining is not enough on its own.
// A DEBUG trap runs before each command and is entitled to read $? from the one
// that just finished; if endap's own handler runs first it replaces that with
// its own result, and the chained trap quietly sees 0 for everything.
func TestBashChainedDebugTrapSeesTheExitStatus(t *testing.T) {
	rc := "HISTCONTROL=ignorespace\n" +
		`trap 'ENDAP_TEST_SEEN="$ENDAP_TEST_SEEN$?"' DEBUG`
	// The variable is cleared from the prompt, after endap has installed its
	// trap. Without that, statuses collected while .bashrc was still running --
	// before endap's trap replaced the probe's -- would satisfy the assertion
	// no matter what endap does afterwards.
	r := runShell(t, "bash", rc, "ENDAP_TEST_SEEN=\nfalse\ntrue\nfalse\necho \"SEEN=$ENDAP_TEST_SEEN\"\n")
	seen := ""
	for _, line := range strings.Split(r.out, "\n") {
		if i := strings.Index(line, "SEEN="); i >= 0 {
			seen = strings.TrimSpace(line[i+len("SEEN="):])
		}
	}
	if seen == "" {
		t.Fatalf("the probe never ran:\n%s", r.out)
	}
	if !strings.Contains(seen, "1") {
		t.Fatalf("the chained DEBUG trap only ever saw 0 (%q); endap is clobbering $? before it runs:\n%s",
			seen, r.out)
	}
}

// TestBashHistcontrol is V10.
func TestBashHistcontrol(t *testing.T) {
	cases := []struct {
		value       string
		wantRepeats int
	}{
		{"", 3},
		{"ignorespace", 3},
		// Declared unsupported: bash does not add the repeat to its history, so
		// there is no history number change for endap to notice.
		{"ignoredups", 1},
		{"erasedups", 1},
		{"ignoreboth", 1},
	}
	for _, c := range cases {
		name := c.value
		if name == "" {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			r := runShell(t, "bash", "HISTCONTROL="+c.value,
				"echo same\necho same\necho same\n echo leading-space\necho end\n")
			if n := countCmd(r, "echo same"); n != c.wantRepeats {
				t.Errorf("HISTCONTROL=%q recorded %d repeats, want %d (recorded %q)",
					c.value, n, c.wantRepeats, r.cmds())
			}
			for _, cmd := range r.cmds() {
				if strings.HasPrefix(cmd, " ") {
					t.Errorf("HISTCONTROL=%q recorded a space-prefixed command", c.value)
				}
			}
		})
	}
}

func TestBashHistcontrolWarning(t *testing.T) {
	r := runShell(t, "bash", "HISTCONTROL=ignoreboth", "true\n")
	if !strings.Contains(r.out, "HISTCONTROL=ignoreboth") {
		t.Fatalf("no warning about a HISTCONTROL that hides commands:\n%s", r.out)
	}
}

func TestNonInteractiveBashDoesNothing(t *testing.T) {
	// .bashrc is read by non-interactive shells too (ssh host 'cmd').
	bin := endap(t)
	dir := t.TempDir()
	script, err := exec.Command(bin, "init", "bash").Output()
	if err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(dir, "rc")
	if err := os.WriteFile(rc, append([]byte("ENDAP_TEST_REACHED_END=0\n"), append(script, []byte("\nENDAP_TEST_REACHED_END=1\n")...)...), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", "-c", ". "+rc+"; echo REACHED=$ENDAP_TEST_REACHED_END; echo PC=${PROMPT_COMMAND:-none}").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "REACHED=0") {
		t.Fatalf("the script ran past its non-interactive guard:\n%s", out)
	}
	if !strings.Contains(string(out), "PC=none") {
		t.Fatalf("a non-interactive shell had its PROMPT_COMMAND changed:\n%s", out)
	}
}

func TestZshSavesThePreviousCtrlRBinding(t *testing.T) {
	r := runShell(t, "zsh", "bindkey '^R' history-incremental-search-backward",
		"echo \"PREV=$ENDAP_PREV_BINDKEY_CTRL_R\"\n")
	if !strings.Contains(r.out, "history-incremental-search-backward") {
		t.Fatalf("the previous Ctrl-R binding was not saved:\n%s", r.out)
	}
}

func TestNoBindkeyLeavesCtrlRAlone(t *testing.T) {
	bin := endap(t)
	out, err := exec.Command(bin, "init", "zsh", "--no-bindkey").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "\nbindkey '^R'") {
		t.Fatalf("--no-bindkey still bound Ctrl-R:\n%s", out)
	}
	if !strings.Contains(string(out), "zle -N __endap_search") {
		t.Fatal("--no-bindkey dropped the widget as well as the binding")
	}
}

// TestNoPlaceholderSurvives guards the substitution in init.go. A syntax check
// cannot catch a leftover @FOO@: in every position these appear in, it is a
// perfectly valid word, so the script parses and fzf receives nonsense.
func TestNoPlaceholderSurvives(t *testing.T) {
	bin := endap(t)
	// Only the @NAME@ shape; a bare @ is legitimate in ${PROMPT_COMMAND[@]}.
	leftover := regexp.MustCompile(`@[A-Z_]+@`)
	for _, sh := range []string{"zsh", "bash", "fish"} {
		for _, opts := range [][]string{
			{}, {"--preview"}, {"--fzf-sort"}, {"--no-bindkey"}, {"--keep-histcontrol"},
			{"--preview", "--fzf-sort", "--no-bindkey", "--keep-histcontrol"},
		} {
			name := sh + " " + strings.Join(opts, " ")
			script, err := exec.Command(bin, append([]string{"init", sh}, opts...)...).Output()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if m := leftover.FindAll(script, -1); m != nil {
				t.Errorf("%s: unreplaced placeholders %q", name, m)
			}
		}
	}
}

func TestGeneratedScriptsParse(t *testing.T) {
	bin := endap(t)
	for _, sh := range []string{"zsh", "bash"} {
		for _, opts := range [][]string{
			{}, {"--preview"}, {"--fzf-sort"}, {"--no-bindkey"},
			{"--preview", "--fzf-sort", "--no-bindkey", "--keep-histcontrol"},
		} {
			name := sh + " " + strings.Join(opts, " ")
			t.Run(name, func(t *testing.T) {
				if _, err := exec.LookPath(sh); err != nil {
					t.Skipf("%s is not installed", sh)
				}
				script, err := exec.Command(bin, append([]string{"init", sh}, opts...)...).Output()
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "script")
				if err := os.WriteFile(path, script, 0o600); err != nil {
					t.Fatal(err)
				}
				if out, err := exec.Command(sh, "-n", path).CombinedOutput(); err != nil {
					t.Fatalf("%s -n failed: %v\n%s\n--- script ---\n%s", sh, err, out, script)
				}
			})
		}
	}
}

// TestDoctorSeesBashHistorySettings covers the checks that can only work
// because the integration republishes bash's history settings under ENDAP_BASH_*
// -- bash exports none of them, so a child process is otherwise blind to the
// very settings that decide whether endap can record anything.
func TestDoctorSeesBashHistorySettings(t *testing.T) {
	cases := []struct {
		name string
		rc   string
		want string
	}{
		{"ignoredups", "HISTCONTROL=ignoredups", "HISTCONTROL contains ignoredups"},
		{"erasedups", "HISTCONTROL=erasedups", "HISTCONTROL contains erasedups"},
		{"histignore", "HISTCONTROL=\nHISTIGNORE='ls*:bg'", "HISTIGNORE"},
		{"histsize zero", "HISTCONTROL=\nHISTSIZE=0", "HISTSIZE=0"},
		{"history off", "HISTCONTROL=\nset +o history", "set +o history"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runShell(t, "bash", c.rc, "endap doctor\n")
			if !strings.Contains(r.out, c.want) {
				t.Fatalf("doctor did not report %q:\n%s", c.want, r.out)
			}
			if !strings.Contains(r.out, "ERROR") {
				t.Fatalf("the finding was not an error:\n%s", r.out)
			}
		})
	}
	t.Run("healthy", func(t *testing.T) {
		r := runShell(t, "bash", "HISTCONTROL=ignorespace", "endap doctor\n")
		for _, line := range strings.Split(r.out, "\n") {
			if strings.Contains(line, "ERROR") && !strings.Contains(line, "fzf") {
				t.Fatalf("doctor reported an error on a healthy bash:\n%s", r.out)
			}
		}
	})
	t.Run("prompt command hijacked", func(t *testing.T) {
		// endap's hooks have to stay first and last. If something appends
		// itself afterwards, $? and the start time stop being reliable, and
		// that is worth saying out loud rather than silently recording wrong
		// numbers.
		r := runShell(t, "bash", "HISTCONTROL=ignorespace", "PROMPT_COMMAND=\"$PROMPT_COMMAND; true\"\nendap doctor\n")
		if !strings.Contains(r.out, "PROMPT_COMMAND") {
			t.Fatalf("doctor did not notice the change to PROMPT_COMMAND:\n%s", r.out)
		}
	})
}
