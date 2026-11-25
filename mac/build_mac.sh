#!/usr/bin/env bash
# build_mac.sh — сборка «Мост.app» на маке (нужны Command Line Tools: swiftc, codesign; Go 1.27+).
#   bash mac/build_mac.sh <версия>
# Ядро most-core (Go, без CGO) + оболочка на Swift (WKWebView, значок в строке меню) → dist/Мост.app,
# одно приложение для Intel и Apple Silicon (M1–M4). Подпись — без сертификата («ad-hoc»): скачанное на другой мак
# откроется только через «Настройки → Конфиденциальность и безопасность → Всё равно открыть»
# (или xattr -dr com.apple.quarantine /Applications/Мост.app).
set -euo pipefail
VER=${1:?версия}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
GO=${GO:-$(command -v go || echo "$HOME/sdk/go/bin/go")}
OUT="$ROOT/dist/Мост.app"; C="$OUT/Contents"
rm -rf "$OUT"; mkdir -p "$C/MacOS" "$C/Resources"
cd "$ROOT"
T=$(mktemp -d)
for a in amd64 arm64; do
  echo "== ядро ($a)"
  GOOS=darwin GOARCH=$a CGO_ENABLED=0 "$GO" build -trimpath -ldflags "-s -w -checklinkname=0" -o "$T/core-$a" ./cmd/most-core
done
lipo -create -output "$C/Resources/most-core" "$T/core-amd64" "$T/core-arm64"
for a in x86_64 arm64; do
  echo "== оболочка ($a)"
  swiftc -O -target $a-apple-macos12.0 -o "$T/shell-$a" mac/MostApp.swift -framework Cocoa -framework WebKit
done
lipo -create -output "$C/MacOS/Most" "$T/shell-x86_64" "$T/shell-arm64"
lipo -info "$C/Resources/most-core" "$C/MacOS/Most"
echo "== иконка"
IS=$(mktemp -d)/Most.iconset; mkdir -p "$IS"
for s in 16 32 128 256 512; do
  sips -z $s $s core/web/icon256.png --out "$IS/icon_${s}x${s}.png" >/dev/null
  d=$((s*2)); sips -z $d $d core/web/icon256.png --out "$IS/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns "$IS" -o "$C/Resources/Most.icns"
cat > "$C/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleName</key><string>Мост</string>
  <key>CFBundleDisplayName</key><string>Мост</string>
  <key>CFBundleIdentifier</key><string>ru.most.app</string>
  <key>CFBundleExecutable</key><string>Most</string>
  <key>CFBundleIconFile</key><string>Most</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$VER</string>
  <key>CFBundleVersion</key><string>$VER</string>
  <key>LSMinimumSystemVersion</key><string>12.0</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSLocalNetworkUsageDescription</key><string>«Мост» подключается к серверам в вашей локальной сети, если вы их готовите или используете.</string>
  <key>NSAppTransportSecurity</key><dict><key>NSAllowsLocalNetworking</key><true/></dict>
</dict></plist>
EOF
codesign --force --deep -s - "$OUT"
echo "== готово: $OUT"; du -sh "$OUT"
