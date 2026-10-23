#!/usr/bin/env bash
set -euo pipefail

# Build both metadata test modes from the same upstream release, including flash.
destination=${1:?Usage: keydb_fixture.sh destination}
source_dir=$(mktemp -d)
trap 'rm -rf "$source_dir"' EXIT
git clone --depth 1 --branch v6.3.4 https://github.com/Snapchat/KeyDB.git "$source_dir/src"
git -C "$source_dir/src" submodule update --init --depth 1 deps/rocksdb
make -C "$source_dir/src" -j4 BUILD_TLS=yes ENABLE_FLASH=yes MALLOC=libc keydb-server
install -m 0755 "$source_dir/src/src/keydb-server" "$destination"
