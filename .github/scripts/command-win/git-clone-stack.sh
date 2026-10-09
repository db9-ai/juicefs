#!/bin/bash
set -euo pipefail

# Diagnose a failed public-fixture clone without replacing its test result.
unset GO_DEPENDENCY_TOKEN GIT_CONFIG_PARAMETERS GIT_CONFIG_COUNT
export GIT_TERMINAL_PROMPT=0
export GIT_TRACE2_EVENT=1 GIT_TRACE_REFS=1
export GIT_TRACE2_ENV_VARS= GIT_TRACE2_CONFIG_PARAMS=

# Debug the native Git binary, not the launcher that starts another process.
git_exec_path=$(cygpath -u "$(git --exec-path)")
git_binary="$git_exec_path/../../bin/git.exe"
debugger=/c/mingw64/bin/gdb.exe
test -x "$git_binary"
test -x "$debugger"
directory=$(mktemp -d /z/git-clone-stack.XXXXXX)
cleanup() {
    local status=$?
    trap - EXIT
    for attempt in 1 2 3; do
        if rm -rf -- "$directory"; then
            exit "$status"
        fi
        echo "Diagnostic directory cleanup attempt $attempt failed" >&2
        sleep 1
    done
    echo "Diagnostic directory remains: $directory" >&2
    if [ "$status" -eq 0 ]; then status=1; fi
    exit "$status"
}
trap cleanup EXIT

"$debugger" --version
# Keep multi-word commands and the spaced Git path out of the native argv parser.
# GDB parses the quoted, forward-slash paths inside this command file itself.
commands="$directory/commands.gdb"
cat > "$commands" <<EOF
set auto-load off
set pagination off
handle SIGSEGV stop print nopass
file "$(cygpath -m "$git_binary")"
set args -c credential.helper= -c http.extraHeader= clone --verbose --progress --no-checkout https://github.com/juicedata/juicefs.git "$(cygpath -m "$directory/repo")" --depth 1
EOF
# Print frames and loaded modules only; do not dump memory or local variables.
cat >> "$commands" <<'EOF'
run
if !$_isvoid($_exitcode)
  printf "Diagnostic Git exited with code %d; no live stack remains.\n", $_exitcode
else
  if !$_isvoid($_exitsignal)
    printf "Diagnostic Git terminated with signal %d; no live stack remains.\n", $_exitsignal
  else
    thread apply all bt
    info sharedlibrary
    x/12i $pc
  end
end
EOF
MSYS2_ARG_CONV_EXCL='*' "$debugger" --batch --nx \
    --command="$(cygpath -m "$commands")"
