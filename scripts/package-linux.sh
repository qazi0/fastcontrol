#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
[[ "$(uname -s)" == Linux ]] || { echo "Build Linux Packages On Linux" >&2; exit 1; }
case "$(uname -m)" in aarch64|arm64) arch=arm64;; x86_64) arch=amd64;; *) echo "Unsupported Linux Architecture" >&2; exit 1;; esac
export GOPATH="${GOPATH:-$HOME/.cache/gopath}"
export GOMODCACHE="${GOMODCACHE:-$GOPATH/pkg/mod}"
export GOCACHE="${GOCACHE:-$HOME/.cache/go-build}"
version=1.9.5
checksum=ff1a9053feb509ff9d7729703355541ae9690073a6b1c40eb692c962e0dc1720
cache="${FASTCONTROL_BUILD_CACHE:-$HOME/.cache/fastcontrol-speech}"
mkdir -p "$cache" dist
archive="$cache/v$version.tar.gz"
folder="$(mktemp -d)"
trap 'rm -rf "$folder"' EXIT
if [[ ! -f "$archive" ]] || [[ "$(sha256sum "$archive" | awk '{print $1}')" != "$checksum" ]]; then
 curl -fL --retry 3 --max-time 600 "https://github.com/ggml-org/whisper.cpp/archive/refs/tags/v$version.tar.gz" -o "$folder/source.tar.gz"
 [[ "$(sha256sum "$folder/source.tar.gz" | awk '{print $1}')" == "$checksum" ]] || { echo "Whisper Source Checksum Failed" >&2; exit 1; }
 mv "$folder/source.tar.gz" "$archive"
fi
source_dir="$cache/whisper.cpp-$version"
[[ -d "$source_dir" ]] || tar -xzf "$archive" -C "$cache"
build="$cache/build-$version-linux-$arch"
cmake -S "$source_dir" -B "$build" \
 -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF \
 -DCMAKE_EXE_LINKER_FLAGS='-static-libstdc++ -static-libgcc' \
 -DWHISPER_BUILD_IS_DEV=OFF -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_SERVER=OFF -DWHISPER_CURL=OFF \
 -DGGML_NATIVE=OFF -DGGML_OPENMP=OFF -DGGML_METAL=OFF -DGGML_CPU_ARM_ARCH=armv8-a \
 -DGGML_AVX=OFF -DGGML_AVX2=OFF -DGGML_FMA=OFF -DGGML_F16C=OFF
cmake --build "$build" --target whisper-cli --parallel 4
mkdir -p "$folder/speech"
install -m 755 "$build/bin/whisper-cli" "$folder/speech/whisper-cli"
cp "$source_dir/LICENSE" "$folder/speech/Whisper-LICENSE.txt"
gcc_major="$(gcc -dumpversion | cut -d. -f1)"
cp "/usr/share/doc/gcc-$gcc_major-base/copyright" "$folder/speech/GCC-Runtime-LICENSE.txt"
cp /usr/share/common-licenses/GPL-3 "$folder/speech/GPL-3.txt"
cp scripts/setup-speech.sh "$folder/speech/setup.sh"
CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags '-s -w' -o "$folder/fastcontrol" .
cp contrib/fastcontrol.service "$folder/"
cp -R licenses "$folder/"
# Inspect the packaged binaries, including speech dependencies, before archiving.
file "$folder/fastcontrol" "$folder/speech/whisper-cli"
ldd "$folder/speech/whisper-cli" | tee "$folder/dependencies.txt"
if rg -q 'not found|libwhisper|libggml|libstdc\+\+|libgcc_s' "$folder/dependencies.txt"; then
 echo "Speech Runtime Has An Unbundled Library" >&2; exit 1
fi
"$folder/fastcontrol" -speech-status >/dev/null
"$folder/speech/whisper-cli" --version >/dev/null
asset="fastcontrol_linux_$arch.tar.gz"
tar -czf "dist/$asset" -C "$folder" fastcontrol fastcontrol.service licenses speech
(cd dist && sha256sum "$asset" > "SHA256SUMS_linux_$arch")
