// Command endap records shell history as an append-only JSONL log and ranks it
// for fzf.
package main

import (
	"fmt"
	"os"

	"github.com/shirou/endap/internal/cli"
)

// version is the endap release. It is a plain constant: builds are reproducible
// without a linker flag, and the value only matters to `endap --version`.
const version = "1.0.0"

const usage = `endap - shell history that piles up

usage: endap <command> [flags]

  init <shell>   print the shell integration script (zsh, bash, fish)
  add            record one history entry (called from a shell hook)
  list           print merged, ranked history for fzf
  export         copy the log to stdout untouched
  doctor         check the installation and the log
  import <src>   read an existing history file (bash, zsh, fish, atuin, endap)
  compact        rebuild the log: drop exact duplicates, apply ignore, park corrupt lines
  forget <re>    delete every record matching a regular expression
  merge <file>   merge logs from other hosts
  stats          summarise the log

Run "endap <command> --help" for the flags of a command.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	env := cli.OSEnv()
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch cmd := args[0]; cmd {
	case "init":
		return cli.Init(env, args[1:])
	case "add":
		return cli.Add(env, args[1:])
	case "list":
		return cli.List(env, args[1:])
	case "export":
		return cli.Export(env, args[1:])
	case "doctor":
		return cli.Doctor(env, args[1:])
	case "import":
		return cli.Import(env, args[1:])
	case "compact":
		return cli.Compact(env, args[1:])
	case "forget":
		return cli.Forget(env, args[1:])
	case "merge":
		return cli.Merge(env, args[1:])
	case "stats":
		return cli.Stats(env, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, usage)
		return 0
	case "-V", "--version", "version":
		fmt.Fprintf(os.Stdout, "endap %s\n", version)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "endap: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}
