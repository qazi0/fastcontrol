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
  cp android/app/build/outputs/apk/release/app-release.apk dist/fastcontrol_android.apk
fi
cd dist
shasum -a 256 fastcontrol_darwin_arm64.tar.gz > SHA256SUMS
[[ ! -f fastcontrol_android.apk ]] || shasum -a 256 fastcontrol_android.apk >> SHA256SUMS
