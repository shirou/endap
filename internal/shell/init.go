// Package shell holds the integration scripts that `endap init` prints.
package shell

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed zsh.zsh
var zshScript string

//go:embed bash.bash
var bashScript string

//go:embed fish.fish
var fishScript string

// Options are the switches `endap init <shell>` accepts.
type Options struct {
	// Preview turns on the tsv output and the fzf preview pane. It requires
	// fzf 0.60 or newer: an older fzf exits with an unknown-option error, which
	// takes Ctrl-R down with it.
	Preview bool
	// FzfSort hands the ordering back to fzf's history scheme instead of
	// pinning it with --no-sort.
	FzfSort bool
	// NoBindkey leaves Ctrl-R alone and only defines the widget.
	NoBindkey bool
	// KeepHistcontrol silences the bash HISTCONTROL warning at shell startup.
	KeepHistcontrol bool
}

// Shells lists the supported shell names.
func Shells() []string {
	names := []string{"bash", "fish", "zsh"}
	sort.Strings(names)
	return names
}

// Script returns the integration script for a shell.
//
// The output is static text. It never probes fzf's version or feature set at
// shell startup: doing so would either cost a process on every shell, or make
// the behaviour change silently underfoot. The flags above pick the variant,
// and `endap doctor` is what checks the installed fzf.
func Script(name string, opt Options) (string, error) {
	var tmpl string
	switch name {
	case "zsh":
		tmpl = zshScript
	case "bash":
		tmpl = bashScript
	case "fish":
		tmpl = fishScript
	default:
		return "", fmt.Errorf("unsupported shell %q (want one of %s)", name, strings.Join(Shells(), ", "))
	}

	sortOpt := "--no-sort"
	if opt.FzfSort {
		// --scheme=history is the history preset: it drops the path-oriented
		// bonuses and keeps the input order among equally scored candidates.
		// It is not the same as --no-sort -- a clearly better match still wins.
		sortOpt = "--scheme=history"
	}
	listFormat, fzfView := "", ""
	if opt.Preview {
		listFormat = " --format tsv"
		// --with-nth also narrows what fzf searches, so the leading columns
		// cannot match the query, and --accept-nth is what keeps the whole tsv
		// row out of the command line on accept.
		fzfView = ` --delimiter='\t' --with-nth=5.. --accept-nth=5..` +
			` --preview 'printf "runs: %s\nlast: %s\ncwd:  %s\n" {2} {3} {4}'`
	}

	r := strings.NewReplacer(
		"@FZF_SORT@", sortOpt,
		"@LIST_FORMAT@", listFormat,
		"@FZF_VIEW@", fzfView,
		"@BINDKEY@", bindkey(name, opt),
		"@HISTCONTROL_CHECK@", histcontrolCheck(name, opt),
	)
	return r.Replace(tmpl), nil
}

// bindkey returns the block that binds Ctrl-R, or an explanation of how to bind
// it by hand when --no-bindkey was given.
//
// Ctrl-R usually already belongs to something -- fzf's own widget, atuin -- so
// the previous binding is saved before it is replaced.
func bindkey(name string, opt Options) string {
	if opt.NoBindkey {
		switch name {
		case "zsh":
			return "# Ctrl-R was left alone. Bind it with: bindkey '^R' __endap_search"
		case "bash":
			return `# Ctrl-R was left alone. Bind it with: bind -x '"\C-r": __endap_search'`
		default:
			return "# Ctrl-R was left alone. Bind it with: bind \\cr __endap_search"
		}
	}
	switch name {
	case "zsh":
		return "typeset -g ENDAP_PREV_BINDKEY_CTRL_R=\"$(bindkey -L '^R')\"\n" +
			"bindkey '^R' __endap_search"
	case "bash":
		// Both listings are needed: fzf and atuin bind Ctrl-R with `bind -x`,
		// which does not appear in `bind -p` at all. The anchor keeps the
		// unrelated \C-x\C-r and \e\C-r out of the saved value.
		return `ENDAP_PREV_BINDKEY_CTRL_R="$( { bind -p; bind -X; } 2>/dev/null | grep -E '^"\\C-r"' || true)"` + "\n" +
			`bind -x '"\C-r": __endap_search'`
	default:
		return "    set -g ENDAP_PREV_BINDKEY_CTRL_R (bind \\cr 2>/dev/null)\n" +
			"    bind \\cr __endap_search"
	}
}

func histcontrolCheck(name string, opt Options) string {
	if name != "bash" || opt.KeepHistcontrol {
		return ""
	}
	// endap takes the command text from "history 1", so anything bash keeps out
	// of its history is invisible to endap as well. HISTCONTROL=ignoreboth is
	// the default in the stock .bashrc, so most bash users see this once and
	// then decide. endap does not edit HISTCONTROL itself: that would be
	// changing the user's environment behind their back.
	return `case ":$HISTCONTROL:" in
  *:ignoredups:*|*:erasedups:*|*:ignoreboth:*)
    printf 'endap: HISTCONTROL=%s makes bash skip repeated commands, so endap cannot record them either. See "endap doctor"; pass --keep-histcontrol to "endap init bash" to silence this.\n' "$HISTCONTROL" >&2
    ;;
esac
`
}
