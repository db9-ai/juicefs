#!/bin/bash -e
source .github/scripts/common/common_win.sh


[[ -z "$META_URL" ]] && META_URL=redis://127.0.0.1:6379/1

test_clone_with_jfs_source()
{
    prepare_win_test
    ./juicefs.exe format $META_URL myjfs
    ./juicefs.exe mount -d $META_URL z:
    ls /z
    if [[ ! -d /z/juicefs ]]; then
        echo "Downloading the clone fixture"
        # This public fixture needs no dependency credentials. Keep them out of traces.
        env -u GO_DEPENDENCY_TOKEN -u GIT_CONFIG_PARAMETERS -u GIT_CONFIG_COUNT \
            GIT_TRACE2_EVENT=1 GIT_TRACE2_ENV_VARS= GIT_TRACE2_CONFIG_PARAMS= \
            GIT_TERMINAL_PROMPT=0 git -c credential.helper= -c http.extraHeader= \
            clone --verbose --progress --no-checkout https://github.com/juicedata/juicefs.git /z/juicefs --depth 1
        echo "Checking out the clone fixture"
        GIT_TRACE2_EVENT=1 git -C /z/juicefs checkout --force HEAD
    fi
    ls /z/juicefs
    do_clone true
    echo "test clone without --preserve"
#    do_clone false
}

do_clone()
{
    is_preserve=$1
    cmd.exe /c "taskkill /F /IM git.exe 2>nul || ver>nul"
    cmd.exe /c "rmdir /s /q z:\juicefs1 2>nul || ver>nul"
    cmd.exe /c "rmdir /s /q z:\juicefs2 2>nul || ver>nul"
    sleep 1
    
    [[ "$is_preserve" == "true" ]] && preserve="--preserve" || preserve=""
    cp -r /z/juicefs /z/juicefs1 $preserve
    ./juicefs.exe clone /z/juicefs /z/juicefs2 $preserve
    diff -ur /z/juicefs1 /z/juicefs2 --no-dereference
 #   CURRENT_DIR=$(pwd)
 #   cmd.exe /c "dir /s /b /a z:\juicefs1" > "${CURRENT_DIR}/log1"
 #   cmd.exe /c "dir /s /b /a z:\juicefs2" > "${CURRENT_DIR}/log2"
 #   diff -u "${CURRENT_DIR}/log1" "${CURRENT_DIR}/log2"
 #   rm -f "${CURRENT_DIR}/log1" "${CURRENT_DIR}/log2"
}

test_clone_with_small_files(){
    prepare_win_test
    ./juicefs.exe format $META_URL myjfs
    ./juicefs.exe mount -d $META_URL z:
    mkdir /z/test
    for i in $(seq 1 2000); do
        echo $i > /z/test/$i
    done
    ./juicefs.exe clone /z/test /z/test1
    diff -ur /z/test1 /z/test1
}

source .github/scripts/common/run_test.sh && run_test $@

