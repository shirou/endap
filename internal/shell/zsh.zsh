# endap shell integration for zsh, produced by `endap init zsh`.

if [[ -o interactive ]]; then

zmodload zsh/datetime 2>/dev/null

# The session id is derived here instead of by a helper subcommand: the PID and
# the shell's start time already identify a session, and deriving it costs no
# process. It never becomes a file path, so endap only checks its shape.
typeset -g ENDAP_SESSION="${ENDAP_SESSION:-$$-${EPOCHSECONDS:-0}}"

typeset -g __endap_cmd=''
typeset -gi __endap_ts=0
typeset -gF __endap_t0=0

__endap_preexec() {
  # $1 is the command line as typed, leading whitespace and newlines intact.
  __endap_cmd="$1"
  __endap_t0=${EPOCHREALTIME:-0}
  __endap_ts=$(( __endap_t0 * 1000 ))
}

__endap_precmd() {
  local ret=$?
  # precmd also fires with no preexec before it: on an empty line, on Ctrl-C at
  # the prompt, on a comment-only line, and on the shell's first prompt. Without
  # this guard and the clearing below, the previous command is recorded again
  # every time one of those happens.
  if [[ -n "$__endap_cmd" ]]; then
    local cmd="$__endap_cmd"
    __endap_cmd=''
    local -a args
    args=( --cwd "$PWD" --exit "$ret" --sess "$ENDAP_SESSION" --sh zsh )
    if (( __endap_ts > 0 )); then
      args+=( --ts "$__endap_ts" )
      local -F now=${EPOCHREALTIME:-0}
      local -i dur=$(( (now - __endap_t0) * 1000 ))
      (( dur >= 0 )) && args+=( --dur "$dur" )
    fi
    # The command goes through the environment, not through an argument:
    # /proc/<pid>/cmdline is world-readable, so an argument would show the whole
    # command to every user on the host for as long as endap runs.
    #
    # Past the threshold it goes on stdin instead: a shell builtin accepts a
    # command line longer than anything execve will carry. The threshold counts
    # characters while the kernel counts bytes -- Linux caps a single argv or
    # environment string at 128KiB regardless of ARG_MAX -- so it is set low
    # enough that even four bytes per character stays well inside that.
    if (( ${#cmd} > 20000 )); then
      print -rn -- "$cmd" | endap add --cmd - "${args[@]}"
    else
      ENDAP_CMD="$cmd" endap add --cmd-env ENDAP_CMD "${args[@]}"
    fi
  fi
  __endap_ts=0
  __endap_t0=0
  return 0
}

autoload -Uz add-zsh-hook
add-zsh-hook preexec __endap_preexec
add-zsh-hook precmd __endap_precmd

__endap_search() {
  local sel
  sel=$(endap list --cwd "$PWD"@LIST_FORMAT@ | fzf @FZF_SORT@ --read0 --print0 \
        --height=40% --layout=reverse@FZF_VIEW@ --query="$LBUFFER")
  if [[ -n "$sel" ]]; then
    BUFFER="${sel%$'\0'}"
    CURSOR=$#BUFFER
  fi
  zle reset-prompt
}
zle -N __endap_search
@BINDKEY@
fi
