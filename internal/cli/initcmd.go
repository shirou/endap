package cli

import (
	"fmt"
	"strings"

	"github.com/shirou/endap/internal/shell"
)

// InitUsage documents the init subcommand.
const InitUsage = "init <zsh|bash|fish> [--preview] [--fzf-sort] [--no-bindkey] [--keep-histcontrol]"

// Init prints the shell integration script.
func Init(env *Env, args []string) int {
	fs := newFlagSet(env, "init", InitUsage)
	var opt shell.Options
	fs.BoolVar(&opt.Preview, "preview", false, "show count, last run and directory in an fzf preview pane (needs fzf 0.60+)")
	fs.BoolVar(&opt.FzfSort, "fzf-sort", false, "let fzf order the candidates with --scheme=history instead of keeping endap's order")
	fs.BoolVar(&opt.NoBindkey, "no-bindkey", false, "define the widget but leave Ctrl-R bound to whatever has it")
	fs.BoolVar(&opt.KeepHistcontrol, "keep-histcontrol", false, "bash: do not warn at startup about HISTCONTROL settings that hide commands from endap")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		env.errf("init needs exactly one shell name (%s)", strings.Join(shell.Shells(), ", "))
		return 2
	}
	script, err := shell.Script(pos[0], opt)
	if err != nil {
		env.errf("%v", err)
		return 2
	}
	fmt.Fprint(env.Stdout, script)
	return 0
}
