#!/usr/bin/env bash
# build_android.sh — сборка «Моста» для Android на сервере (2026-11-15).
#   bash build_android.sh <версия, напр. 0.8.0> <код версии, напр. 80>
# 1) ядро Go → app/libs/most.aar (gomobile bind; arm64, armv7, x86_64 — эмулятор);
# 2) иконки из web/icon256.png; 3) Gradle 8.10.2 (качается один раз в /opt/gradle) → APK, подпись ключом выпуска
#    (/root/most-android/release.jks; пароли в /root/most-android/signing.properties — вне git).
set -euo pipefail
VER=${1:?версия}; CODE=${2:?код версии}
export HOME=/root GOPATH=/root/go GOTOOLCHAIN=go1.27.0 ANDROID_HOME=/opt/android-sdk
export ANDROID_NDK_HOME=$ANDROID_HOME/ndk/27.2.12479018 PATH=/root/go/bin:$ANDROID_HOME/cmdline-tools/latest/bin:$PATH
A=$(cd "$(dirname "$0")" && pwd); ROOT=$(dirname "$A")   # корень репозитория
cd $ROOT
echo "== ядро → most.aar"
gomobile bind -target=android/arm64,android/arm,android/amd64 -androidapi 26 -trimpath -ldflags "-s -w -checklinkname=0" -o $A/app/libs/most.aar ./mobile
echo "== иконки"
for d in xhdpi:96 xxhdpi:144 xxxhdpi:192; do convert core/web/icon256.png -resize ${d#*:}x${d#*:} $A/app/src/main/res/mipmap-${d%:*}/ic_launcher.png; done
echo "== ключ подписи"
K=/root/most-android; install -d -m700 $K
if [ ! -s $K/release.jks ]; then
  P=$(head -c 24 /dev/urandom | base64 | tr -d '/+=' | cut -c1-24)
  keytool -genkeypair -keystore $K/release.jks -alias most -keyalg RSA -keysize 3072 -validity 10000 \
    -storepass "$P" -keypass "$P" -dname "CN=Most, O=Most" >/dev/null 2>&1
  printf 'storeFile=%s\nstorePassword=%s\nkeyAlias=most\nkeyPassword=%s\n' "$K/release.jks" "$P" "$P" > $K/signing.properties
  chmod 600 $K/*
fi
cp $K/signing.properties $A/signing.properties
echo "== Gradle"
G=/opt/gradle/gradle-8.10.2/bin/gradle
if [ ! -x $G ]; then
  install -d /opt/gradle && cd /tmp && curl -sSLo gradle.zip https://services.gradle.org/distributions/gradle-8.10.2-bin.zip
  echo "$(curl -sSL https://services.gradle.org/distributions/gradle-8.10.2-bin.zip.sha256)  gradle.zip" | sha256sum -c - && unzip -q gradle.zip -d /opt/gradle && rm gradle.zip
fi
printf 'sdk.dir=%s\n' "$ANDROID_HOME" > $A/local.properties
cd $A && $G --no-daemon -q assembleRelease -PmostVersionName=$VER -PmostVersionCode=$CODE
OUT=$A/app/build/outputs/apk/release/app-release.apk
$ANDROID_HOME/build-tools/35.0.0/apksigner verify --print-certs $OUT | head -3
cp $OUT /root/build/most-$VER.apk && ls -la /root/build/most-$VER.apk && sha256sum /root/build/most-$VER.apk
echo ANDROID-BUILD-DONE
