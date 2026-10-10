#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
export GOPATH="${GOPATH:-$HOME/.cache/gopath}"
export GOMODCACHE="${GOMODCACHE:-$GOPATH/pkg/mod}"
export GOCACHE="${GOCACHE:-$HOME/.cache/go-build}"
mkdir -p dist
for arch in arm64 amd64; do
  archive_arch="$arch"
  [[ "$arch" != amd64 ]] || archive_arch=x86_64
  folder="$(mktemp -d)"
  scripts/build-speech.sh "$archive_arch" "$folder/speech"
  cp scripts/setup-speech.sh "$folder/speech/setup.sh"
  GOOS=darwin GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o "$folder/fastcontrol" .
  cp contrib/com.fastcontrol.plist "$folder/"
  cp -R licenses "$folder/"
  tar -czf "dist/fastcontrol_darwin_${archive_arch}.tar.gz" -C "$folder" fastcontrol com.fastcontrol.plist licenses speech
  rm -rf "$folder"
done
if [[ -f android/app/build/outputs/apk/release/app-release.apk ]]; then
  cp android/app/build/outputs/apk/release/app-release.apk dist/fastcontrol_android.apk
fi
cd dist
shasum -a 256 fastcontrol_darwin_*.tar.gz > SHA256SUMS
[[ ! -f fastcontrol_android.apk ]] || shasum -a 256 fastcontrol_android.apk >> SHA256SUMS
