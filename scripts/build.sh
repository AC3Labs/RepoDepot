#!/bin/sh
# Builds the server, the agents for every platform, and a Docker build
# context in ./dist. Usage: scripts/build.sh
set -e
cd "$(dirname "$0")/.."
V=$(cat VERSION)
R=$(git rev-parse --short HEAD 2>/dev/null || echo dev)
rm -rf dist && mkdir -p dist/agents
LD="-s -w -X repodepot/internal/server.Version=$V"
ALD="-s -w -X repodepot/internal/agent.Version=$V"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$LD" -o dist/repodepot ./cmd/repodepot
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$ALD" -o dist/agents/repodepot-agent-linux-amd64 ./cmd/repodepot-agent
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$ALD" -o dist/agents/repodepot-agent-linux-arm64 ./cmd/repodepot-agent
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$ALD" -o dist/agents/repodepot-agent-windows-amd64.exe ./cmd/repodepot-agent
cp internal/server/web/agent/install.sh internal/server/web/agent/install.ps1 dist/agents/
sed -e "s/^ARG VERSION=.*/ARG VERSION=$V/" -e "s/^ARG REVISION=.*/ARG REVISION=$R/" Dockerfile > dist/Dockerfile
ls -la dist dist/agents
