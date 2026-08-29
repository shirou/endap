# endap shell integration for fish, produced by `endap init fish`.

if status is-interactive

    if not set -q ENDAP_SESSION
        set -g ENDAP_SESSION "$fish_pid-"(date +%s)
    end

    set -g __endap_ts ""

    function __endap_preexec --on-event fish_preexec
        # fish has no built-in epoch variable, so the start time costs one fork
        # here. Second resolution is enough for ts; dur comes from
        # $CMD_DURATION, which fish measures itself.
        set -g __endap_ts (date +%s)"000"
    end

    function __endap_postexec --on-event fish_postexec
        set -l st $status
        set -l cmd $argv[1]
        # Same guard as the zsh and bash hooks. With no command there is nothing
        # to record, and an empty list would shift the arguments below.
        if test -z "$cmd"
            set -g __endap_ts ""
            return
        end
        set -l args --cwd $PWD --exit $st --sess $ENDAP_SESSION --sh fish
        if set -q CMD_DURATION
            set -a args --dur $CMD_DURATION
        end
        if test -n "$__endap_ts"
            set -a args --ts $__endap_ts
        end
        set -g __endap_ts ""
        # The command goes through the environment, not through an argument:
        # /proc/<pid>/cmdline is world-readable, so an argument would show the
        # whole command to every user on the host for as long as endap runs.
        #
        # Past the threshold it goes on stdin instead. The threshold counts
        # characters while the kernel counts bytes -- Linux caps a single argv
        # or environment string at 128KiB regardless of ARG_MAX -- so it is set
        # low enough that even four bytes per character stays inside that.
        if test (string length -- "$cmd") -gt 20000
            printf '%s' "$cmd" | endap add --cmd - $args
        else
            ENDAP_CMD="$cmd" endap add --cmd-env ENDAP_CMD $args
        end
    end

    function __endap_search
        set -l sel (endap list --cwd $PWD@LIST_FORMAT@ \
            | fzf @FZF_SORT@ --read0 --print0 --height=40% --layout=reverse@FZF_VIEW@ \
                  --query=(commandline -b -c) | string split0)
        # `string split0` makes fish split the substitution on NUL rather than
        # on newlines. For most selections the newline split and commandline's
        # newline join cancel out, but a command that ends in a newline loses it
        # -- fish strips trailing newlines from a command substitution.
        if test -n "$sel"
            commandline -r -- $sel
        end
        commandline -f repaint
    end
@BINDKEY@
end
