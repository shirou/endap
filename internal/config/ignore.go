package config

import (
	"fmt"
	"regexp"
	"strings"
)

// DefaultIgnore are the patterns applied before any config file is read.
//
// The set in the spec, /(PASSWORD|SECRET|TOKEN|API_KEY)=/, is case-sensitive
// and assumes an "=", so it misses "export gh_token=...", "--password foo",
// "mysql -phunter2" and "Authorization: Bearer ...". These add the
// case-insensitive and "="-free shapes. They are a net, not a guarantee -- see
// the README.
var DefaultIgnore = []string{
	// The assignment form. No \b before the keyword on purpose: it would stop
	// "gh_token=" and "AWS_SECRET_ACCESS_KEY=" from matching, since _ is a word
	// character.
	`(?i)(password|passwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|credentials?)\s*=\s*\S`,
	// The header form, scoped to HTTP clients. Accepting "key: value" anywhere
	// would drop ordinary prose -- "git commit -m 'update token: handling'",
	// "grep -r 'secret:' ." -- and losing real commands is worse for a history
	// tool than missing an unusual secret.
	`(?i)\b(curl|wget|http|https|xh|httpie)\b.*\b(authorization|api[_-]?key|apikey|x-[a-z-]*key|token|cookie)\s*:\s*\S`,
	`(?i)--pass(word)?[= ]\S`,
	// Scoped to the mysql family on purpose. The bare form "-p\S" also matches
	// "mkdir -pv", "ssh -p2222", "docker run -p8080:80" and "tar -pxf", and
	// silently dropping those from history would be worse than the leak it
	// prevents. The password-adjacent commands keep the protection.
	`(?i)\b(mysql|mysqldump|mysqladmin|mariadb|mariadb-dump)\b.*\s-p\S`,
	`(?i)\bBearer\s+\S`,
	// Issued tokens always carry their separator, and requiring it is what keeps
	// these from matching inside ordinary words. Without the "_" and "-" and the
	// word boundaries, "gh[pousr]" hits "apt-get install ghostscript-doc" and
	// "sk-" hits "git clone .../task-management-system".
	`\bgh[pousr]_[A-Za-z0-9]{20,}\b`,
	`\bsk-[A-Za-z0-9_-]{20,}\b`,
	`\bAKIA[0-9A-Z]{12,}\b`,
	`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`,
	// Without this, "endap forget hunter2" is itself recorded, so the command
	// that removes a secret writes a fresh copy of it.
	`^\s*endap\s+(forget|import)\b`,
}

// Ignore is a compiled set of record-suppressing patterns.
type Ignore struct {
	patterns []*regexp.Regexp
	sources  []string
}

// Match reports whether cmd should be kept out of the log.
func (ig *Ignore) Match(cmd string) bool {
	for _, re := range ig.patterns {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}

// Sources returns the pattern strings in the order they are applied, for doctor.
func (ig *Ignore) Sources() []string { return append([]string(nil), ig.sources...) }

// Len returns the number of active patterns.
func (ig *Ignore) Len() int { return len(ig.patterns) }

// ignoreExpr turns one config value into a regular expression source.
//
// A value wrapped in slashes is a regular expression; anything else is a prefix
// match, which is what the spec's "ignore = exit" form means.
func ignoreExpr(value string) string {
	v := strings.TrimRight(value, " \t")
	if strings.HasPrefix(v, "/") {
		v = v[1:]
		if strings.HasSuffix(v, "/") {
			v = v[:len(v)-1]
		}
		return v
	}
	return "^" + regexp.QuoteMeta(v)
}

// buildIgnore compiles the default patterns plus the user's.
//
// Compilation failures are fail-closed: a broken user pattern is reported and
// dropped, and the defaults always survive. Failing open would silently admit
// every secret the user thought they had excluded.
func buildIgnore(userValues []string) (*Ignore, []string) {
	ig := &Ignore{}
	var warnings []string
	add := func(expr, src string, isDefault bool) {
		re, err := regexp.Compile(expr)
		if err != nil {
			what := "ignore pattern"
			if isDefault {
				what = "built-in ignore pattern"
			}
			warnings = append(warnings, fmt.Sprintf("%s %q did not compile and was dropped: %v", what, src, err))
			return
		}
		ig.patterns = append(ig.patterns, re)
		ig.sources = append(ig.sources, expr)
	}
	for _, p := range DefaultIgnore {
		add(p, p, true)
	}
	for _, v := range userValues {
		add(ignoreExpr(v), v, false)
	}
	return ig, warnings
}
