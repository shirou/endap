package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{"30d", 30 * 24 * time.Hour, false},
		{"1y", 365 * 24 * time.Hour, false},
		{"2w", 14 * 24 * time.Hour, false},
		{"12h", 12 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"1.5d", 36 * time.Hour, false},
		{"500ms", 500 * time.Millisecond, false},
		{"", 0, true},
		{"-1d", 0, true},
		{"nonsense", 0, true},
		// time.ParseDuration accepts none of the units the config is written in.
		{"30", 0, true},
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("ParseDuration(%q) = %v, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDuration(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sort != "recent" || cfg.CwdBoost != 2.0 {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestParserSplitsOnTheFirstEqualsOnly(t *testing.T) {
	// The pattern below contains "=". Splitting on every "=" would truncate it
	// to "/PASSWORD" and the ignore would silently stop matching what the user
	// wrote.
	cfg, err := Load(write(t, "ignore = /PASSWORD=[a-z]+/\ncwd_boost = 7.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CwdBoost != 7.0 {
		t.Fatalf("cwd_boost = %v", cfg.CwdBoost)
	}
	if !cfg.Ignore.Match("export PASSWORD=hunter") {
		t.Fatal("the pattern with an = in it did not match")
	}
	if cfg.Ignore.Match("export PASSWORD=") {
		t.Fatal("the pattern matched more than it should")
	}
}

func TestCommentsOnlyAtTheStartOfALine(t *testing.T) {
	// "ignore = /^#/" is a legitimate setting, so a "#" mid-line is data.
	cfg, err := Load(write(t, "# a comment\n  # an indented comment\nignore = /^#/\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Ignore.Match("# a shell comment") {
		t.Fatal("the /^#/ pattern was mangled by comment stripping")
	}
}

func TestPrefixAndRegexpIgnoreForms(t *testing.T) {
	cfg, err := Load(write(t, "ignore = exit\nignore = /^(ls|cd|pwd)$/\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"exit", "exit 1", "ls", "cd", "pwd"} {
		if !cfg.Ignore.Match(c) {
			t.Errorf("%q should be ignored", c)
		}
	}
	for _, c := range []string{"my-exit", "ls -la", "cdx"} {
		if cfg.Ignore.Match(c) {
			t.Errorf("%q should not be ignored", c)
		}
	}
}

func TestBrokenPatternIsFailClosed(t *testing.T) {
	cfg, err := Load(write(t, "ignore = /[unclosed/\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Critical) == 0 {
		t.Fatal("a pattern that will not compile was accepted silently")
	}
	// The built-ins have to survive a broken user pattern; failing open would
	// let every secret through.
	if !cfg.Ignore.Match("export API_KEY=deadbeef") {
		t.Fatal("the built-in patterns were dropped along with the broken one")
	}
}

func TestDefaultIgnoreCoversTheCommonSecretShapes(t *testing.T) {
	cfg := Defaults()
	secret := []string{
		"export API_KEY=deadbeefdeadbeef",
		"export gh_token=abc",
		"AWS_SECRET_ACCESS_KEY=xyz aws s3 ls",
		"curl -H 'Authorization: Bearer eyJhbGciOi'",
		"mysql -uroot -phunter2",
		"echo ghp_0123456789abcdefghij",
		"aws configure set x AKIAIOSFODNN7EXAMPLE",
		"curl --password s3cret https://example.com",
		"endap forget hunter2",
		"endap import bash",
		"deploy --api-key=abcdef",
		"export OPENAI_API_KEY=sk-proj-0123456789abcdefghijklmn",
		"slack-cli --token xoxb-1234567890-abcdefghij",
		// The header form still has to be caught for HTTP clients.
		"curl -H 'X-Api-Key: s3cretvalue' https://example.com",
		"wget --header 'Authorization: token abc123' https://example.com",
	}
	for _, c := range secret {
		if !cfg.Ignore.Match(c) {
			t.Errorf("%q was not ignored", c)
		}
	}
	// These must keep working. For a history tool a false positive is a command
	// that silently never gets recorded, which is worse than missing an unusual
	// secret -- and every one of these was dropped by an earlier version of the
	// patterns above.
	ordinary := []string{
		// "-p" followed by anything would eat these.
		"mkdir -pv /tmp/x", "ssh -p2222 host", "docker run -p8080:80 nginx",
		"tar -pxf archive.tar",
		// An unanchored "gh[pousr]" or "sk-" hits ordinary words: ghostscript,
		// task-management, disk-usage.
		"apt-get install ghostscript-doc", "brew install ghostscript",
		"git clone https://github.com/acme/task-management-system",
		"docker run task-management-app", "go get github.com/x/disk-usage-tool",
		"npm install @scope/sk-something-lib", "make disk-usage",
		// Accepting "keyword:" anywhere hits prose.
		"git commit -m 'update token: handling'", "grep -r 'secret:' .",
		"echo 'my password: foo'",
		"git push", "ls -la", "grep -rn token .",
		"echo my token is in the vault", "vim ~/.aws/credentials",
	}
	for _, c := range ordinary {
		if cfg.Ignore.Match(c) {
			t.Errorf("%q was ignored but should not be", c)
		}
	}
}

func TestNonFiniteTunablesAreRejected(t *testing.T) {
	// ParseFloat accepts "Inf" and "NaN" as ordinary values, and either one
	// reaching the score turns the ranking comparison into something sort.Slice
	// is not allowed to be given.
	for _, v := range []string{"Inf", "+Inf", "-Inf", "Infinity", "inf", "NaN", "nan"} {
		cfg, err := Load(write(t, "cwd_boost = "+v+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.CwdBoost != 2.0 {
			t.Errorf("cwd_boost = %v was accepted (%v)", v, cfg.CwdBoost)
		}
		if len(cfg.Warnings) == 0 {
			t.Errorf("cwd_boost = %v was rejected without a warning", v)
		}
	}
	// An overflowing literal is caught by ParseFloat itself, but check it too so
	// the two rejection paths stay covered.
	cfg, err := Load(write(t, "cwd_boost = 1e400\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CwdBoost != 2.0 || len(cfg.Warnings) == 0 {
		t.Errorf("cwd_boost = 1e400 was accepted (%v)", cfg.CwdBoost)
	}
}

func TestHalflifeIsRejectedByName(t *testing.T) {
	// Anyone who tuned the old exponential decay has this key in their config.
	// The generic unknown-key warning would leave them believing it still
	// shapes the ranking.
	cfg, err := Load(write(t, "halflife = 7d\n"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cfg.Warnings, " ")
	if !strings.Contains(joined, "halflife") || !strings.Contains(joined, "time bands") {
		t.Fatalf("no warning explaining halflife: %v", cfg.Warnings)
	}
}

func TestRetentionIsRejectedWithAnExplanation(t *testing.T) {
	cfg, err := Load(write(t, "retention = 365d\n"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cfg.Warnings, " ")
	if !strings.Contains(joined, "retention") {
		t.Fatalf("no warning about retention: %v", cfg.Warnings)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("ENDAP_SORT", "rank")
	t.Setenv("ENDAP_CWD_BOOST", "3.5")
	cfg, err := Load(write(t, "sort = recent\ncwd_boost = 2.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sort != "rank" || cfg.CwdBoost != 3.5 {
		t.Fatalf("environment did not override the file: %+v", cfg)
	}
}

func TestEnvOverridesEveryTunable(t *testing.T) {
	t.Setenv("ENDAP_FAIL_PENALTY", "0.25")
	t.Setenv("ENDAP_SHORT_PENALTY", "0.1")
	t.Setenv("ENDAP_SHORT_LEN", "5")
	t.Setenv("ENDAP_IGNORE", "/^deploy /")
	cfg, err := Load(write(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FailPenalty != 0.25 || cfg.ShortPenalty != 0.1 || cfg.ShortLen != 5 {
		t.Fatalf("environment was not applied: %+v", cfg)
	}
	if !cfg.Ignore.Match("deploy production") {
		t.Fatal("ENDAP_IGNORE was not added to the pattern set")
	}
	if !cfg.Ignore.Match("export API_KEY=deadbeef") {
		t.Fatal("ENDAP_IGNORE replaced the built-in patterns instead of adding to them")
	}
}

func TestBadEnvValueWarnsAndKeepsTheDefault(t *testing.T) {
	t.Setenv("ENDAP_SHORT_LEN", "not-a-number")
	t.Setenv("ENDAP_SORT", "nonsense")
	cfg, err := Load(write(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ShortLen != 3 || cfg.Sort != "recent" {
		t.Fatalf("a bad environment value overwrote the default: %+v", cfg)
	}
	joined := strings.Join(cfg.Warnings, " ")
	for _, want := range []string{"ENDAP_SHORT_LEN", "ENDAP_SORT"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning naming %s: %v", want, cfg.Warnings)
		}
	}
}

func TestHostOverride(t *testing.T) {
	t.Setenv("ENDAP_HOST", "thinkpad")
	if Host() != "thinkpad" {
		t.Fatalf("Host() = %q", Host())
	}
}
