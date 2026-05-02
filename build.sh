#!/usr/bin/env bash

set -e

APP=network_monitor_tool
mkdir -p build

echo "Building Linux..."
GOOS=linux GOARCH=amd64 go build -o build/$APP-linux-amd64

echo "Building Windows..."
GOOS=windows GOARCH=amd64 go build -o build/$APP-windows-amd64.exe

echo "Building macOS Intel..."
GOOS=darwin GOARCH=amd64 go build -o build/$APP-darwin-amd64

echo "Building macOS ARM..."
GOOS=darwin GOARCH=arm64 go build -o build/$APP-darwin-arm64

echo "Building FreeBSD..."
GOOS=freebsd GOARCH=amd64 go build -o build/$APP-freebsd-amd64

echo "Done!"