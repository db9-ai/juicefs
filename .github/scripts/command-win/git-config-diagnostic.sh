#!/bin/bash
set -euo pipefail

# Compare clone's branch configuration writes on local and mounted storage.
# This runs only after a failed clone and does not replace the original test.
unset GO_DEPENDENCY_TOKEN GIT_CONFIG_PARAMETERS GIT_CONFIG_COUNT
export GIT_TRACE2_EVENT=1 GIT_TRACE2_ENV_VARS= GIT_TRACE2_CONFIG_PARAMS=

result=0
for root in "${RUNNER_TEMP:?}" /z; do
    directory=$(mktemp -d "$root/git-config-diagnostic.XXXXXX")
    config="$directory/config"
    printf '[core]\n\trepositoryformatversion = 0\n[remote "origin"]\n\turl = https://github.com/juicedata/juicefs.git\n' > "$config"
    for key in branch.main.remote branch.main.merge; do
        value=origin
        [[ "$key" != branch.main.merge ]] || value=refs/heads/main
        echo "CONFIG_WRITE_START root=$root key=$key"
        if git config --file "$config" --replace-all "$key" "$value"; then
            echo "CONFIG_WRITE_OK root=$root key=$key"
        else
            status=$?
            echo "CONFIG_WRITE_FAILED root=$root key=$key status=$status"
            result=1
        fi
    done
    # Only this generated, credential-free configuration is printed.
    cat "$config"
    rm -rf -- "$directory"
done
exit "$result"
