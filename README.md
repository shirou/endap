# endap

Shell history that piles up, and that you can dig back out with fzf.

The name is Indonesian for *to settle, to sediment*. Commands accumulate in
layers and stay there.

`HISTFILE` throws away the things you actually want when you go looking: which
directory you were in, whether the command worked, how long it took, which
machine you were on. endap keeps all of it, in one append-only JSONL file per
user, and merges the files from several machines into one history.

```
$ endap list | fzf --scheme=history --read0 --print0
```

- One process per command, about 3ms. No daemon, no database, no index.
- Newest first, the way Ctrl-R works everywhere else. Frecency is there when
  you want it, behind `--sort rank`.
- Multi-line commands survive intact, which is why the log is JSON.
- Standard library only, no cgo. A stripped binary is under 3MiB.

---

## Install

```sh
go install github.com/shirou/endap@latest
```

You also need [fzf](https://github.com/junegunn/fzf). See
[fzf versions](#fzf-versions) for what each version buys you.

## Set up

Add one line to your shell's startup file, then start a new shell.

```sh
# ~/.zshrc
eval "$(endap init zsh)"

# ~/.bashrc
eval "$(endap init bash)"

# ~/.config/fish/config.fish
endap init fish | source
```

Then check the installation:

```sh
endap doctor
```

`eval` costs a few milliseconds per shell start. If that matters to you, write
the output to a file and source that instead:

```sh
endap init zsh > ~/.endap.zsh    # re-run after upgrading endap
echo 'source ~/.endap.zsh' >> ~/.zshrc
```

### `endap init` flags

| Flag | What it does |
|---|---|
| `--preview` | Show run count, last use and directory in an fzf preview pane. **Needs fzf 0.60+** |
| `--no-fzf-sort` | Keep endap's order untouched (`--no-sort`) instead of letting fzf rank the matches |
| `--no-bindkey` | Define the widget but leave Ctrl-R bound to whatever already has it |
| `--keep-histcontrol` | bash only: stop warning at startup about `HISTCONTROL` settings that hide commands |

`--fzf-sort` used to be how you asked for `--scheme=history`. That is the
default now, so the flag still parses and does nothing: a startup line that
still carries it keeps working rather than breaking the shell it is eval'd in.

The output is static text. endap never probes fzf's version or feature set at
shell startup: that would cost a process on every shell, or make the behaviour
change silently underfoot. Pick the variant with a flag, and let `endap doctor`
check what you have installed.

### endap takes over Ctrl-R

Unless you pass `--no-bindkey`, `endap init` rebinds Ctrl-R and will overwrite
fzf's own `fzf-history-widget`, atuin's binding, or whatever else has it. The
previous binding is saved into `$ENDAP_PREV_BINDKEY_CTRL_R` so you can see what
was there. On bash that covers both readline function bindings and the `bind -x`
form that fzf and atuin actually use.

## Use

Press Ctrl-R. Whatever you have already typed becomes the initial query, so it
behaves the way fzf's and atuin's bindings do.

Candidates leave endap newest first, and fzf runs with `--scheme=history`: the
best matches rise to the top, and candidates fzf scores equally keep the order
they arrived in. Both halves are load-bearing.

fzf matches a subsequence, not a substring. `ping` therefore also matches
`scp admin@host:...ardupilot.tlog logs` through its p, i, n and g, and on a
2000-command history 216 candidates matched that way while only 29 contained the
word. Ranking is what puts the real `ping` lines on top of those; keeping the
input order among equals is what puts the most recent one first.

`--no-fzf-sort` pins the order instead, with fzf's `--no-sort`, so fzf filters
and never reorders. What that buys: type `gs` with `git status`, `gs` and
`git stash` in your history, and fzf's match score puts the exact match `gs`
first, however long ago you last ran it, while `--no-sort` leaves `git status`
where its recency put it. What it costs is the paragraph above -- a short query
scatters the matches you want through the ones you don't.

```
$ endap stats
records               48213
unique commands       3187
first record          2024-11-02T09:14:22+09:00
last record           2026-08-29T10:41:03+09:00
failed commands       2841 (5.9%)
mean duration         218ms
log size              9.4 MiB
hosts                 thinkpad: 41022
                      workstation: 7191
```

## Ordering

Newest first, with one entry per command placed by its most recent run. That is
what Ctrl-R does in bash, zsh, fish, fzf and atuin, and it is what a history
tool has to do before it is allowed to be clever.

```sh
endap list --sort rank
```

The Ctrl-R widget calls plain `endap list`, so `sort = rank` in the config file
is what changes the order there. `--sort` on the command line overrides it.

`rank` scores each command as:

```
score = log(count + 1) × recency
```

Counting runs logarithmically keeps 500 runs of `ls` from permanently burying
the one `ffmpeg` invocation you spent ten minutes assembling last week.

`recency` is zoxide's aging curve, moved from directories to commands:

| Last run | Multiplier |
|---|---|
| Within the hour | × 4 |
| Within the day | × 2 |
| Within the week | × 0.5 |
| Older | × 0.25 |

Sixteen-fold across a single week, which is where a shell session lives. The
first implementation decayed exponentially from a 30-day half-life instead; that
loses 2.3% in a day, which leaves the time term a near-constant next to
`log(count + 1)` and the ranking as frequency wearing frecency's name. A step
function is cruder and spends its whole range where the range is needed.

Three corrections apply on top:

| Condition | Multiplier |
|---|---|
| The command last exited non-zero | × 0.5 |
| It has ever been run in the current directory | × 2.0 |
| It is 3 characters or shorter | × 0.3 |

The directory boost needs `--cwd`, which the shell integration always passes,
and counts *how many* runs happened there rather than just where the last one
was. If it only looked at the last directory, then running `make test` once in
`/tmp` would cancel the boost for the fifty times you ran it in your project.

## Importing an existing history

```sh
endap import zsh                       # ~/.zsh_history
endap import bash  --file ~/.bash_history
endap import fish
endap import endap --file other-host.jsonl
endap import atuin --file atuin.tsv
```

Imports are idempotent, including for history files with no timestamps. If the
source has a command *n* times and the log already has it *m* times, the import
adds `max(0, n-m)` copies — so running it twice adds nothing, and re-running it
after the file has grown adds only the new part. Run counts survive, which the
simpler "skip anything already present" rule would have destroyed for a
timestamp-free `~/.bash_history`.

Idempotency holds within one version of the parser, and the zsh one changed
once. Earlier versions read the history file without undoing zsh's metafication,
so every non-ASCII command came in mangled. Those records do not match what the
current parser produces, so a fresh `endap import zsh` adds the decoded copy
*beside* the mangled one rather than deduplicating against it. If your log has
commands that came in through an older `import zsh`:

```sh
endap forget '\x{fffd}'          # dry run; prints record ids, not the commands
endap forget '\x{fffd}' --yes    # then delete them
endap import zsh                 # and take them in again, decoded this time
```

The pattern matches the replacement character the mangled bytes turned into. A
real command containing U+FFFD would go with them; `--show-matches` prints what
would.

The ignore list (below) applies to imports. This is where it matters most: a
`~/.bash_history` is where years of accidentally-typed secrets live, and an
import is the one moment they would all be copied in at once.

Imported records get `host: "imported"` and no session id. Use `--host` to
override.

### atuin

atuin keeps its history in a database, and endap does not embed one, so the data
has to come out through atuin itself:

```sh
atuin history list --format "{time}\t{exit}\t{duration}\t{directory}\t{command}" > atuin.tsv
endap import atuin --file atuin.tsv
```

Running `endap import atuin` with no `--file` runs that command for you. A
command containing a newline cannot survive the tab-separated intermediate; that
is a limit of the format, not of the importer.

## Syncing between machines

endap has no sync server. Put `history.jsonl` in Syncthing, git, Dropbox, or
rsync it around.

```sh
endap merge ~/sync/other-host.jsonl --in-place
```

Merging deduplicates by record id and sorts by timestamp, so it is
order-independent and repeatable. If git leaves you with a conflict, keep both
sides and run them through `endap merge`.

**Deletions can come back.** This follows from file sync, not from a bug in
endap: anything you remove with `compact` or `forget` will reappear if a machine
that still has the old version syncs it back to you. Run `forget` on every
machine, or accept that the record is out there.

**Check the permissions on the other side.** endap creates `history.jsonl` as
`0600`, but it only sets that at creation time and git does not record file
modes. A log that arrives on a new machine through `git clone` gets whatever
your umask says, which on most systems is world-readable — for a file holding
every command you have ever typed. `endap doctor` reports it; run it once after
setting up a new machine.

## Files

Everything lives in `$XDG_DATA_HOME/endap` (`~/.local/share/endap` by default,
or wherever `ENDAP_DATA_DIR` points), created `0700` with files `0600`.

| File | What it is |
|---|---|
| `history.jsonl` | The log |
| `history.jsonl.rej` | Lines endap could not parse, parked rather than deleted |
| `history.jsonl.lock` | Held while `compact`, `forget` or `merge --in-place` runs |
| `history.jsonl.tmp.*` | A rewrite in progress. Left behind means one was interrupted |

All four are inside the synced directory, so `.rej` travels with the log.

`list`, `stats`, `compact`, `forget` and `merge` read the whole log into memory,
so their footprint grows with it — roughly the file size plus a slice header per
line. `add` appends and never reads, so your prompt is unaffected no matter how
large the log gets. `endap doctor` tells you when the log is big enough to be
worth compacting.

Configuration lives in `$XDG_CONFIG_HOME/endap/config`.

## Configuration

`key = value`, one per line. `#` starts a comment only at the beginning of a
line, and the value runs to the end of the line — both because `ignore = /^#/`
and patterns containing `=` have to work.

```
sort         = recent
cwd_boost    = 2.0
fail_penalty = 0.5
short_penalty = 0.3
short_len    = 3

# Prefix match, or /regexp/
ignore = exit
ignore = /^(ls|cd|pwd)$/
ignore = /AWS_SECRET/
```

Every setting can be overridden from the environment: `ENDAP_SORT`,
`ENDAP_CWD_BOOST`, `ENDAP_FAIL_PENALTY`, `ENDAP_SHORT_PENALTY`,
`ENDAP_SHORT_LEN`, `ENDAP_IGNORE`, plus `ENDAP_DATA_DIR`, `ENDAP_CONFIG` and
`ENDAP_HOST`.

## Keeping secrets out

Three rules keep a command out of the log, and none of them produce any output —
skipping is normal operation, not a failure:

1. It is empty or only whitespace.
2. It starts with a space (the `HIST_IGNORE_SPACE` convention).
3. It matches an `ignore` pattern.

The hooks hand the command to endap through an environment variable rather than
an argument. `/proc/<pid>/cmdline` is world-readable, so an argument would show
every command in full to every user on the machine for as long as `endap add`
runs — and for a shell builtin like `export TOKEN=...`, which spawned no process
at all before endap was installed, that is an exposure endap would be creating.
`/proc/<pid>/environ` is readable only by you and root. The ignore list cannot
help with this either way: by the time endap can look at a pattern, the
arguments have already been visible.

If the disk fills up mid-record, endap terminates the fragment so it reads back
as one corrupt line instead of swallowing the next record too. That terminator
is a separate write and takes no lock, so a second shell appending in the same
instant can still land inside the corrupt line and be lost with it. `endap
doctor` counts corrupt lines, which is how you find out.

The built-in patterns cover the common shapes: `PASSWORD=`, `--password x`,
`Authorization: Bearer …`, `mysql -phunter2`, `ghp_…`, `sk-…`, `AKIA…`, `xoxb-…`,
and `endap forget` itself — without that last one, the command that removes a
secret writes a fresh copy of it. `endap doctor` prints the full list.

**They are a net, not a guarantee.** A secret in an unusual shape gets through.
If one does, `endap forget` removes it:

```sh
endap forget 'hunter2'          # shows what matches, deletes nothing
endap forget 'hunter2' --yes    # deletes
```

The dry run prints record ids and never the commands themselves; use
`--show-matches` when you want to see them. `forget` reaches `history.jsonl`,
`.rej` and any leftover `.tmp` files on this machine. It does not reach freed
disk blocks, copies already synced to other machines, or your own backups.

A pattern that will not compile is dropped and reported, and the built-in
patterns always survive. Failing the other way would let through exactly the
secrets you thought you had excluded.

## Compaction

Nothing runs automatically. `endap doctor` says when the log is getting big, and
you decide.

```sh
endap compact          # says what it would do
endap compact --yes    # does it
```

Compaction removes exact duplicates, applies ignore patterns added since the
records were written, and moves unparseable lines to `.rej`. It does not fold
old records into aggregates: two machines compacting the same synced log would
each produce an aggregate with its own id, `merge` deduplicates by id, and the
run counts would be double-counted with no way to tell.

There is no `.bak`. The rewrite is already atomic, and a backup file would keep
a copy of exactly the secret you just added an ignore pattern to remove.

While a rewrite runs it holds `history.jsonl.lock`, so two of them cannot
destroy each other's work. `add` and `list` never look at that lock — your
prompt is never blocked by a compaction.

Records appended during a rewrite are picked up in a catch-up pass, and once
more after the rename. Two windows stay open, both measured in microseconds:
a write that is in flight at the moment of the rename, and a rewrite that is
killed between the rename and that last recovery pass. Closing either would mean
taking a lock on every prompt, or keeping the old file under a recovery name
across the whole operation.

## fzf versions

| Version | What you get |
|---|---|
| **0.33** | Required. The widget passes `--scheme=history`, which arrived here; below this fzf exits with an unknown-option error and takes Ctrl-R with it. `--no-fzf-sort` builds a widget that does without it |
| 0.53 | Recommended. Below this, multi-line commands are squashed onto one line |
| 0.60 | Required for `--preview`. Below this, `endap init <shell> --preview` makes fzf exit with an unknown-option error and takes Ctrl-R down with it |

`endap doctor` checks all three.

## bash: settings that hide commands from endap

In bash, endap takes the command text from `history 1`. `$BASH_COMMAND` cannot
be used: the DEBUG trap fires once per simple command, so a pipeline arrives in
pieces, and `$BASH_COMMAND` strips the leading space that is supposed to keep a
command out of the log entirely.

That makes bash's own history settings decide what endap can see:

| Setting | Effect |
|---|---|
| `HISTCONTROL` contains `ignoredups`, `erasedups` or `ignoreboth` | **Not supported.** bash does not record a repeated command, so endap cannot either, and run counts are wrong |
| `HISTIGNORE` is non-empty | **Not supported.** Matching commands never reach bash's history |
| `HISTSIZE=0`, `set +o history` | **Not supported.** Nothing is recorded at all |
| `HISTCONTROL` contains `ignorespace` | Fine, and it is what the leading-space rule expects |

`HISTCONTROL=ignoreboth` is the default in the stock `~/.bashrc` on most
distributions, so most bash users will see this warning the first time. endap
does not edit `HISTCONTROL` for you — changing your environment behind your back
is worse than telling you about it. Remove `ignoredups`/`ignoreboth` yourself, or
pass `--keep-histcontrol` to accept the loss and silence the warning.

`endap init bash` inserts itself at the front and back of `PROMPT_COMMAND`
(handling both the string and the bash 5.1 array form) and chains any DEBUG trap
you already had.

Two things need a newer bash than the 3.2 that ships with macOS. Command
durations need `$EPOCHREALTIME` (bash 5.0), so on older bash `dur` is left out.
The Ctrl-R widget needs `READLINE_LINE` and `READLINE_POINT` (bash 4.0) to read
what you have typed and to put the selection back: on bash 3.2 the key still
opens fzf, but the query is not pre-filled and the choice is not inserted.
Recording works throughout. Install a newer bash — Homebrew's, say — if you want
the search.

## Uninstalling

endap never writes to `~/.zsh_history` or `~/.bash_history`, so your old history
setup is exactly where you left it and works the moment you take endap out.

1. Remove the `eval "$(endap init …)"` line from your shell startup file.
2. Start a new shell. That restores Ctrl-R and your DEBUG trap; both are only
   changed for the current session.
3. Back the log up if you want it: `endap export > endap-backup.jsonl`.
4. Delete `~/.local/share/endap/` and `~/.config/endap/`.

## Commands

```
endap init <shell>     print the shell integration script
endap add              record one entry (called from a shell hook)
endap list             print the merged history for fzf, newest first
endap export           copy the log to stdout untouched
endap doctor           check the installation and the log
endap import <source>  read an existing history file
endap compact          rebuild the log
endap forget <regexp>  delete every record matching a pattern
endap merge <file>...  merge logs from other machines
endap stats            summarise the log
```

`endap add` always exits 0. It is the only subcommand a shell hook calls, and a
non-zero status there would break your prompt, or your shell under `set -e`.
Everything else exits non-zero on failure, `doctor` included — otherwise it
could not be used as a check.

Failures are reported on stderr rather than swallowed. A history tool that
silently stops recording is not discovered until the day you go looking for a
command that was never saved.

## Record format

One JSON object per line.

```json
{"v":1,"id":"06CEVYB0FD813MQ5D0966XHDXM","ts":1756339200123,"dur":842,"cmd":"git rebase -i HEAD~3","cwd":"/home/shirou/src/endap","host":"thinkpad","sess":"4021-1756339100","exit":0,"sh":"zsh"}
```

| Field | Meaning |
|---|---|
| `v` | Schema version |
| `id` | 26 characters, sortable by time, unique across machines |
| `ts` | Start time, Unix milliseconds |
| `dur` | Duration in milliseconds, omitted when unknown |
| `cmd` | The command, exactly as typed, newlines and all |
| `cwd` | Working directory |
| `host` | Machine name, or `ENDAP_HOST` |
| `sess` | Shell session, derived from PID and shell start time |
| `exit` | Exit status, omitted when unknown |
| `sh` | `zsh`, `bash`, `fish` |

Records from a newer endap are passed through by `compact` and `merge` untouched
rather than deleted, because a machine running a newer version is the normal
state of a synced setup. Lines that cannot be parsed at all are moved to `.rej`,
never dropped.

That guarantee holds as long as later versions only *add* fields. An older endap
still has to parse a newer record to see its `v`, so a version that changed the
type of an existing field, or stopped writing a flat object, would look corrupt
to it and be moved to `.rej` instead of passed through.

Commands are stored as valid UTF-8. A command containing invalid UTF-8 bytes has
those bytes replaced, because the writer is `encoding/json`.

## License

Apache License 2.0. See [LICENSE](LICENSE).
