# endap shell integration for bash, produced by `endap init bash`.

# .bashrc is read by non-interactive shells too (ssh host 'cmd'), where there is
# no prompt to hook and nothing to record.
case $- in
  *i*) ;;
  *) return 0 ;;
esac

if [ -z "${ENDAP_SESSION:-}" ]; then
  if [ -n "${EPOCHSECONDS:-}" ]; then
    ENDAP_SESSION="$$-$EPOCHSECONDS"
  else
    ENDAP_SESSION="$$-$(date +%s 2>/dev/null)"
  fi
fi

__endap_armed=0
__endap_ran=0
__endap_t0=
__endap_exit=0
__endap_histnum=
__endap_prev_debug=
__endap_pc_array=0

# Capture any DEBUG trap already installed, here at the top level.
#
# This cannot be done inside the install function below: a DEBUG trap is not in
# scope for a function unless functrace is on, so `trap -p DEBUG` there reports
# nothing and the existing handler is silently discarded instead of chained.
__endap_prev_debug_spec="$(trap -p DEBUG)"

# __endap_dbg records the start time. It deliberately does not read
# $BASH_COMMAND for the command text: DEBUG fires once per simple command, so a
# pipeline would arrive in pieces, and $BASH_COMMAND drops the leading space
# that tells endap not to record a command at all.
__endap_dbg() {
  local LC_NUMERIC=C
  if [ "$__endap_armed" = 1 ]; then
    __endap_t0=${EPOCHREALTIME:-}
    __endap_armed=0
    __endap_ran=1
  fi
  return 0
}

# __endap_prev_dbg runs the DEBUG trap that was already installed.
#
# The eval is the first statement, and this runs before endap's own handler in
# the trap, because a DEBUG trap is entitled to read $? from the command that
# just finished. Anything endap does first -- even a test -- replaces that with
# its own result, and the chained handler silently starts seeing 0 for every
# command. Only installed when there is something to chain, so there is no guard
# here to get in the way.
__endap_prev_dbg() {
  eval "$__endap_prev_debug"
}

# __endap_pre is the first entry in PROMPT_COMMAND.
#
# It has three jobs, all of which have to happen before anything else runs.
# It captures $? before another PROMPT_COMMAND hook (starship, direnv) replaces
# it. It disarms the DEBUG trap so that the hooks that follow it cannot
# overwrite the start time -- DEBUG fires for the contents of PROMPT_COMMAND
# too, which otherwise makes every command look like it took a millisecond. And
# it writes the record for the command that just finished.
__endap_pre() {
  __endap_exit=$?
  __endap_armed=0
  if [ "$__endap_ran" = 1 ]; then
    __endap_ran=0
    __endap_record "$__endap_exit"
  fi
  # Hand the user's exit status on to the rest of PROMPT_COMMAND unchanged.
  return $__endap_exit
}

# __endap_arm is the last entry in PROMPT_COMMAND. Re-arming here rather than in
# __endap_pre is what keeps an empty line from producing a ghost record: the
# DEBUG fired by __endap_pre itself would otherwise set __endap_ran again.
__endap_arm() {
  __endap_armed=1
  # These are not exported by bash, so `endap doctor` run as a child process
  # cannot see them. They decide what bash keeps in its own history, and
  # therefore what endap is able to record at all, so doctor has to be able to
  # check them. Refreshed every prompt because they can be changed at any time;
  # all builtins, no process started.
  export ENDAP_BASH_HISTCONTROL="${HISTCONTROL-}"
  export ENDAP_BASH_HISTIGNORE="${HISTIGNORE-}"
  export ENDAP_BASH_HISTSIZE="${HISTSIZE-}"
  if shopt -qo history 2>/dev/null; then
    export ENDAP_BASH_HISTORY=on
  else
    export ENDAP_BASH_HISTORY=off
  fi
  export ENDAP_BASH_PROMPT_COMMAND_OK=1
  __endap_check_prompt_command
  return 0
}

# __endap_check_prompt_command notices when another tool has appended itself
# after endap's last hook or prepended itself before the first. Either one
# breaks the arrangement that keeps $? and the start time intact.
__endap_check_prompt_command() {
  local first last
  # Whether PROMPT_COMMAND is an array was settled at install time. Asking again
  # here would mean a "$(declare -p ...)" on every prompt, and ${var@a} is a
  # parse error on the bash 3.2 that ships with macOS.
  if [ "$__endap_pc_array" = 1 ]; then
    first="${PROMPT_COMMAND[0]}"
    last="${PROMPT_COMMAND[${#PROMPT_COMMAND[@]}-1]}"
  else
    first="${PROMPT_COMMAND%%;*}"
    last="${PROMPT_COMMAND##*;}"
  fi
  first="${first#"${first%%[![:space:]]*}"}"
  last="${last#"${last%%[![:space:]]*}"}"
  last="${last%"${last##*[![:space:]]}"}"
  if [ "$first" != "__endap_pre" ] || [ "$last" != "__endap_arm" ]; then
    export ENDAP_BASH_PROMPT_COMMAND_OK=0
  fi
  return 0
}

__endap_record() {
  local status=$1
  # HISTTIMEFORMAT is an arbitrary strftime string and cannot be parsed back out
  # in general (%n even puts a newline in it), so it is emptied for this call
  # and what is left to strip is exactly the history number.
  local HISTTIMEFORMAT=
  local raw lead num rest
  raw=$(history 1)
  [ -z "$raw" ] && return 0
  lead="${raw%%[![:space:]]*}"
  rest="${raw#"$lead"}"
  num="${rest%%[![:digit:]]*}"
  [ -z "$num" ] && return 0
  rest="${rest#"$num"}"
  # bash prints history entries as "%5d  %s": exactly two spaces follow the
  # number, and everything after them is the command -- including a leading
  # space, which is what tells endap not to record it. With "shopt -s lithist"
  # the command keeps its real newlines, so only the first line is touched.
  rest="${rest#  }"
  if [ "$num" = "$__endap_histnum" ]; then
    # The history number did not move, so bash added no new entry: the command
    # started with a space under HISTCONTROL=ignorespace, or matched HISTIGNORE.
    # Either way there is nothing new to record.
    return 0
  fi
  __endap_histnum=$num

  local ts= dur= now t0 s0 u0 s1 u1
  if [ -n "$__endap_t0" ]; then
    local LC_NUMERIC=C
    t0=${__endap_t0//,/.}
    case "$t0" in
      *.*) s0=${t0%%.*}; u0=${t0#*.} ;;
      *) s0=$t0; u0=0 ;;
    esac
    u0="${u0}000000"; u0="${u0:0:6}"
    ts=$(( s0 * 1000 + 10#$u0 / 1000 ))
    now=${EPOCHREALTIME:-}
    if [ -n "$now" ]; then
      now=${now//,/.}
      case "$now" in
        *.*) s1=${now%%.*}; u1=${now#*.} ;;
        *) s1=$now; u1=0 ;;
      esac
      u1="${u1}000000"; u1="${u1:0:6}"
      dur=$(( (s1 - s0) * 1000 + (10#$u1 - 10#$u0) / 1000 ))
      [ "$dur" -lt 0 ] && dur=
    fi
  fi

  local -a args
  args=( --cwd "$PWD" --exit "$status" --sess "$ENDAP_SESSION" --sh bash )
  [ -n "$ts" ] && args+=( --ts "$ts" )
  [ -n "$dur" ] && args+=( --dur "$dur" )
  # The command goes through the environment, not through an argument:
  # /proc/<pid>/cmdline is world-readable, so an argument would show the whole
  # command to every user on the host for as long as endap runs.
  #
  # Past the threshold it goes on stdin instead: a shell builtin accepts a
  # command line longer than anything execve will carry. The threshold counts
  # characters while the kernel counts bytes -- Linux caps a single argv or
  # environment string at 128KiB regardless of ARG_MAX -- so it is set low
  # enough that even four bytes per character stays well inside that.
  if [ "${#rest}" -gt 20000 ]; then
    printf '%s' "$rest" | endap add --cmd - "${args[@]}"
  else
    ENDAP_CMD="$rest" endap add --cmd-env ENDAP_CMD "${args[@]}"
  fi
  return 0
}

# Chain whatever DEBUG trap is already installed instead of replacing it.
__endap_install_debug() {
  local spec="$__endap_prev_debug_spec"
  if [ -n "$spec" ]; then
    spec="${spec#trap -- }"
    spec="${spec% DEBUG}"
    # The text left over is the handler in shell-quoted form. Letting the shell
    # unquote it is more reliable than undoing '\'' by hand.
    if ! eval "__endap_prev_debug=$spec" 2>/dev/null; then
      printf 'endap: cannot chain the existing DEBUG trap; command duration will not be recorded\n' >&2
      __endap_prev_debug=
      return 1
    fi
  fi
  # The trap excludes endap's own first hook by name. Comparing $BASH_COMMAND
  # against $PROMPT_COMMAND does not work: DEBUG fires per simple command, so it
  # never sees the whole of PROMPT_COMMAND.
  if [ -n "$__endap_prev_debug" ]; then
    trap '__endap_prev_dbg; [[ "$BASH_COMMAND" == "__endap_pre" ]] || __endap_dbg' DEBUG
  else
    trap '[[ "$BASH_COMMAND" == "__endap_pre" ]] || __endap_dbg' DEBUG
  fi
  return 0
}

__endap_install_prompt_command() {
  local decl flags pc
  decl="$(declare -p PROMPT_COMMAND 2>/dev/null)"
  flags="${decl#declare }"
  flags="${flags%% *}"
  case "$flags" in
    *a*)
      # bash 5.1+ array form. Assigning a string to it would overwrite element 0
      # and leave the rest of the array in place.
      __endap_pc_array=1
      PROMPT_COMMAND=( __endap_pre "${PROMPT_COMMAND[@]}" __endap_arm )
      return 0
      ;;
  esac
  pc="${PROMPT_COMMAND:-}"
  while :; do
    case "$pc" in
      *";"|*" "|*"	"|*"
") pc="${pc%?}" ;;
      *) break ;;
    esac
  done
  if [ -n "$pc" ]; then
    PROMPT_COMMAND="__endap_pre;${pc};__endap_arm"
  else
    PROMPT_COMMAND="__endap_pre;__endap_arm"
  fi
  return 0
}

if [ -n "${EPOCHREALTIME:-}" ]; then
  __endap_install_debug
else
  # $EPOCHREALTIME arrived in bash 5.0. Without a millisecond clock there is
  # nothing for the DEBUG trap to record, so it is left alone entirely.
  printf 'endap: bash %s has no $EPOCHREALTIME; command duration will not be recorded\n' "${BASH_VERSION%%(*}" >&2
fi
__endap_install_prompt_command
@HISTCONTROL_CHECK@
__endap_search() {
  local sel
  # A process substitution rather than "$(...)": bash drops NUL bytes from
  # command substitution and prints "warning: ignored null byte in input" every
  # single time, which would be on screen at every Ctrl-R.
  IFS= read -r -d '' sel < <(endap list --cwd "$PWD"@LIST_FORMAT@ \
    | fzf @FZF_SORT@ --read0 --print0 --height=40% --layout=reverse@FZF_VIEW@ \
          --query="${READLINE_LINE:0:${READLINE_POINT:-0}}")
  if [ -n "$sel" ]; then
    READLINE_LINE="$sel"
    READLINE_POINT=${#READLINE_LINE}
  fi
}
@BINDKEY@
