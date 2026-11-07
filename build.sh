#!/bin/bash
# 编译到 bin/music163-mcp；交叉编译时设置 GOOS/GOARCH，如 GOOS=darwin GOARCH=arm64 bash build.sh
set -e
cd "$(dirname "$0")"

VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT_SHA=$(git rev-parse --short HEAD 2>/dev/null || echo "")

LDFlags="-s -w -X 'main.versionName=${VERSION}' -X 'main.commitSHA=${COMMIT_SHA}'"

suffix=""
if [ "${GOOS:-$(go env GOOS)}" = "windows" ]; then
  suffix=".exe"
fi

mkdir -p bin
CGO_ENABLED=0 go build -trimpath -ldflags "${LDFlags}" -o "bin/music163-mcp${suffix}" ./cmd/music163-mcp
echo "bin/music163-mcp${suffix} (${VERSION})"
