#!/bin/sh
# Build the release binaries: static Linux builds for amd64 and arm64, and
# their SHA256SUMS, into dist/.
#
#   sh scripts/build.sh 0.2.0
set -eu

VERSION="${1:?usage: scripts/build.sh <version>}"
cd "$(dirname "$0")/.."
rm -rf dist
mkdir -p dist

for ARCH in amd64 arm64; do
    CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath \
        -ldflags "-s -w -X main.version=$VERSION" \
        -o "dist/laika-agent-linux-$ARCH" ./cmd/laika-agent
done

(cd dist && sha256sum laika-agent-linux-* > SHA256SUMS)
cp packaging/install.sh dist/install.sh
cat dist/SHA256SUMS
