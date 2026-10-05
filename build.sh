#!/usr/bin/env bash
# Local cross-build; matches the targets in .github/workflows/build.yml.

set -e

APP=ups-monitor
mkdir -p build

build() {
    local name=$1 goos=$2 goarch=$3 goarm=$4 ext=$5
    echo "Building $name..."
    GOOS=$goos GOARCH=$goarch GOARM=$goarm CGO_ENABLED=0 \
        go build -trimpath -ldflags "-s -w" -o "build/$APP-$name$ext" .
}

build linux-amd64   linux   amd64
build linux-arm64   linux   arm64
build linux-armv7   linux   arm 7
build linux-armv6   linux   arm 6
build linux-386     linux   386
build linux-riscv64 linux   riscv64
build windows-amd64 windows amd64 "" .exe
build windows-arm64 windows arm64 "" .exe
build windows-386   windows 386   "" .exe
build darwin-amd64  darwin  amd64
build darwin-arm64  darwin  arm64
build freebsd-amd64 freebsd amd64
build freebsd-arm64 freebsd arm64
build openbsd-amd64 openbsd amd64
build openbsd-arm64 openbsd arm64
build netbsd-amd64  netbsd  amd64
build netbsd-arm64  netbsd  arm64
build dragonfly-amd64 dragonfly amd64
build illumos-amd64 illumos amd64
build solaris-amd64 solaris amd64

echo "Done!"
