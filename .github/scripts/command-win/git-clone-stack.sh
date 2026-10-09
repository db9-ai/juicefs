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
trap 'rm -rf -- "$directory"' EXIT

"$debugger" --version
# Print frames and loaded modules only; do not dump memory or local variables.
"$debugger" --batch --nx \
    -iex 'set auto-load off' \
    -ex 'set pagination off' \
    -ex 'handle SIGSEGV stop print nopass' \
    -ex run \
    -ex 'thread apply all bt' \
    -ex 'info sharedlibrary' \
    -ex 'x/12i $pc' \
    --args "$(cygpath -w "$git_binary")" \
    -c credential.helper= -c http.extraHeader= \
    clone --verbose --progress --no-checkout \
    https://github.com/juicedata/juicefs.git \
    "$(cygpath -w "$directory/repo")" --depth 1
