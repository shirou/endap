package cli

import (
	"io"
	"os"
)

// ExportUsage documents the export subcommand.
const ExportUsage = "export"

// Export copies the log to stdout untouched.
//
// Nothing is parsed, reordered or filtered: this is the backup path, so lines
// endap itself would reject have to come out the other side intact.
func Export(env *Env, args []string) int {
	fs := newFlagSet(env, "export", ExportUsage)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if rejectPositional(env, fs, args) {
		return 2
	}
	path, err := historyPath()
	if err != nil {
		env.errf("%v", err)
		return 1
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		env.errf("cannot read %s: %v", path, err)
		return 1
	}
	defer f.Close()
	if _, err := io.Copy(env.Stdout, f); err != nil {
		env.errf("cannot write the output: %v", err)
		return 1
	}
	return 0
}
