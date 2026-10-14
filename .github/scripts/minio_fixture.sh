#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
case "${1:-}" in
    image)
        version=${2:-RELEASE.2022-01-25T19-56-04Z}
        image="juicefs-ci-minio:$version"
        if docker image inspect "$image" >/dev/null 2>&1; then
            exit 0
        fi
        source_dir=$(mktemp -d)
        trap 'rm -rf "$source_dir"' EXIT
        git clone --depth 1 --branch "$version" https://github.com/minio/minio.git "$source_dir/src"
        mkdir "$source_dir/image"
        (
            cd "$source_dir/src"
            CGO_ENABLED=0 go build -trimpath -o "$source_dir/image/minio" .
        )
        cp /etc/ssl/certs/ca-certificates.crt "$source_dir/image/"
        docker build -t "$image" -f "$script_dir/minio.Dockerfile" "$source_dir/image"
        ;;
    mc)
        destination=${2:-./mc}
        version=${3:-RELEASE.2022-01-07T06-01-38Z}
        build_dir=$(mktemp -d)
        trap 'rm -rf "$build_dir"' EXIT
        GOBIN="$build_dir" go install "github.com/minio/mc@$version"
        install -m 0755 "$build_dir/mc" "$destination"
        ;;
    *)
        echo "Usage: $0 image [version] | mc [destination] [version]" >&2
        exit 2
        ;;
esac
