#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
export GOPATH="${GOPATH:-$HOME/.cache/gopath}"
export GOMODCACHE="${GOMODCACHE:-$GOPATH/pkg/mod}"
export GOCACHE="${GOCACHE:-$HOME/.cache/go-build}"
mkdir -p dist
folder="$(mktemp -d)"
trap 'rm -rf "$folder"' EXIT
scripts/build-speech.sh arm64 "$folder/speech"
cp scripts/setup-speech.sh "$folder/speech/setup.sh"
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o "$folder/fastcontrol" .
cp contrib/com.fastcontrol.plist "$folder/"
cp -R licenses "$folder/"
tar -czf dist/fastcontrol_darwin_arm64.tar.gz -C "$folder" fastcontrol com.fastcontrol.plist licenses speech
if [[ -f android/app/build/outputs/apk/release/app-release.apk ]]; then
  apk_version="$(sed -n 's/.*versionName = "\([^"]*\)".*/\1/p' android/app/build.gradle.kts)"
  [[ -n "$apk_version" ]]
  apk_name="fastcontrol_android_v${apk_version}.apk"
  cp android/app/build/outputs/apk/release/app-release.apk "dist/$apk_name"
fi
cd dist
shasum -a 256 fastcontrol_darwin_arm64.tar.gz > SHA256SUMS
[[ -z "${apk_name:-}" ]] || shasum -a 256 "$apk_name" >> SHA256SUMS
