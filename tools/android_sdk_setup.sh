#!/usr/bin/env bash
# android_sdk_setup.sh — набор для сборки «Моста» под Android на сервере (2026-11-15).
# JDK 17, Android SDK (cmdline-tools с dl.google.com), platform 35, build-tools, NDK; gomobile.
# Запуская скрипт, вы принимаете лицензию Android SDK от Google (sdkmanager спросит её при установке).
# Журнал — /root/build/android-sdk.log.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive HOME=/root GOPATH=/root/go
exec >>/root/build/android-sdk.log 2>&1
echo "=== $(date -Is)"
apt-get install -y -q openjdk-17-jdk-headless unzip >/dev/null
SDK=/opt/android-sdk; install -d $SDK
if [ ! -x $SDK/cmdline-tools/latest/bin/sdkmanager ]; then
  URL=$(curl -s https://dl.google.com/android/repository/repository2-3.xml | grep -o 'commandlinetools-linux-[0-9]*_latest.zip' | sort -t- -k3 -n | tail -1)
  echo "cmdline-tools: $URL"
  cd /tmp && curl -sSLo clt.zip "https://dl.google.com/android/repository/$URL" && rm -rf clt && unzip -q clt.zip -d clt
  install -d $SDK/cmdline-tools && rm -rf $SDK/cmdline-tools/latest && mv clt/cmdline-tools $SDK/cmdline-tools/latest && rm -rf clt clt.zip
fi
export ANDROID_HOME=$SDK PATH=$SDK/cmdline-tools/latest/bin:$PATH
# лицензия Android SDK: sdkmanager спрашивает её при установке
(yes || true) | sdkmanager --install "platform-tools" "platforms;android-35" "build-tools;35.0.0" "ndk;27.2.12479018" >/dev/null || true
[ -d $SDK/ndk/27.2.12479018 ] && [ -d $SDK/platforms/android-35 ] || { echo "SDK не установился"; exit 1; }
echo "sdk: $(ls $SDK)"
GOTOOLCHAIN=go1.27.0 go install golang.org/x/mobile/cmd/gomobile@latest
GOTOOLCHAIN=go1.27.0 go install golang.org/x/mobile/cmd/gobind@latest
ANDROID_NDK_HOME=$SDK/ndk/27.2.12479018 /root/go/bin/gomobile init
echo "ANDROID-SDK-DONE"
