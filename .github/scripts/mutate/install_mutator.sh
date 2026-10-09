#!/usr/bin/env bash
set -euo pipefail

# The upstream tool pins a go/packages version incompatible with current Go.
source_dir=$(mktemp -d)
trap 'rm -rf "$source_dir"' EXIT
git -C "$source_dir" init -q
git -C "$source_dir" fetch --depth 1 https://github.com/zimmski/go-mutesting.git 6d9217011a005762bbcf4ac7a60237dc6a99887f
git -C "$source_dir" checkout --detach FETCH_HEAD
cd "$source_dir"
go get golang.org/x/tools/go/packages@v0.47.0
go install ./cmd/go-mutesting
