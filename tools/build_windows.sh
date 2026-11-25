#!/usr/bin/env bash
# build_windows.sh — сборка «Моста» для Windows (на Linux или в Git Bash; CGO не нужен).
#   bash tools/build_windows.sh <версия, напр. 0.8.0>
# Иконка и сведения о файле — ресурс .syso (go-winres), сам exe — в dist/most-<версия>.exe.
# Версию в программе задаёт `var version` в core/app.go — поменяйте её до сборки.
set -euo pipefail
VER=${1:?версия}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT/cmd/most"
GOTOOLCHAIN=${GOTOOLCHAIN:-auto} go run github.com/tc-hib/go-winres@v0.3.3 simply --arch amd64 \
  --icon "$ROOT/core/web/icon256.png" --manifest gui --product-name "Мост" \
  --file-description "Мост — подключение к рабочему серверу" --product-version "$VER" --file-version "$VER" \
  --original-filename most.exe
cd "$ROOT" && mkdir -p dist
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -H windowsgui -buildid=" -o "dist/most-$VER.exe" ./cmd/most
ls -la "dist/most-$VER.exe"
