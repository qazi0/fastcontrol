#!/bin/bash
set -euo pipefail

arch="${1:?Pass arm64 Or x86_64}"
destination="${2:?Pass The Output Directory}"
case "$arch" in arm64) metal=ON;; x86_64) metal=OFF;; *) exit 1;; esac
version=1.9.5
checksum=ff1a9053feb509ff9d7729703355541ae9690073a6b1c40eb692c962e0dc1720
cache="${FASTCONTROL_BUILD_CACHE:-$HOME/.cache/fastcontrol-speech}"
mkdir -p "$cache" "$destination"
archive="$cache/v$version.tar.gz"
if [[ ! -f "$archive" ]] || [[ "$(shasum -a 256 "$archive" | awk '{print $1}')" != "$checksum" ]]; then
  download="$(mktemp "$cache/source.XXXXXX")"
  trap 'rm -f "$download"' EXIT
  curl -fL --retry 3 --max-time 600 "https://github.com/ggml-org/whisper.cpp/archive/refs/tags/v$version.tar.gz" -o "$download"
  [[ "$(shasum -a 256 "$download" | awk '{print $1}')" == "$checksum" ]] || { echo "Whisper Source Checksum Failed" >&2; exit 1; }
  mv "$download" "$archive"
fi
source_dir="$cache/whisper.cpp-$version"
[[ -d "$source_dir" ]] || tar -xzf "$archive" -C "$cache"
build="$cache/build-$version-$arch"
cmake -S "$source_dir" -B "$build" \
  -DCMAKE_BUILD_TYPE=Release -DCMAKE_OSX_ARCHITECTURES="$arch" \
  -DCMAKE_SYSTEM_NAME=Darwin -DCMAKE_SYSTEM_PROCESSOR="$arch" \
  -DCMAKE_OSX_DEPLOYMENT_TARGET=13.3 -DBUILD_SHARED_LIBS=OFF \
  -DWHISPER_BUILD_IS_DEV=OFF -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_SERVER=OFF -DWHISPER_CURL=OFF \
  -DGGML_NATIVE=OFF -DGGML_OPENMP=OFF -DGGML_METAL="$metal" \
  -DGGML_METAL_EMBED_LIBRARY=ON -DGGML_AVX=OFF -DGGML_AVX2=OFF \
  -DGGML_FMA=OFF -DGGML_F16C=OFF
cmake --build "$build" --target whisper-cli --parallel 4
install -m 755 "$build/bin/whisper-cli" "$destination/whisper-cli"
cp "$source_dir/LICENSE" "$destination/Whisper-LICENSE.txt"
# Static project libraries and embedded Metal shaders avoid Homebrew runtime paths.
if otool -L "$destination/whisper-cli" | tail -n +2 | awk '{print $1}' | grep -Ev '^(/usr/lib/|/System/Library/)' | grep -q .; then
  echo "Speech Runtime Has An Unbundled Library" >&2; exit 1
fi
