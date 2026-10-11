#!/bin/bash
set -euo pipefail

repo="qazi0/fastcontrol"
version="${FASTCONTROL_VERSION:-latest}"
skip_speech="${FASTCONTROL_SKIP_SPEECH:-0}"
case "${1:-}" in --skip-speech) skip_speech=1;; "") ;; *) echo "Usage: install.sh [--skip-speech]" >&2; exit 1;; esac
if [[ "$(uname -s)" != Darwin ]]; then echo "fastcontrol requires macOS." >&2; exit 1; fi
mac_version="$(sw_vers -productVersion)"
mac_major="${mac_version%%.*}"
mac_minor="${mac_version#*.}"; mac_minor="${mac_minor%%.*}"
if (( mac_major < 13 || (mac_major == 13 && mac_minor < 3) )); then echo "fastcontrol requires macOS 13.3 or newer." >&2; exit 1; fi
[[ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" == 1 ]] || { echo "fastcontrol Supports Apple Silicon Macs Only" >&2; exit 1; }
arch=arm64
temp="$(mktemp -d)"
trap 'rm -rf "$temp"' EXIT
asset="fastcontrol_darwin_${arch}.tar.gz"
if [[ "$version" == latest ]]; then base="https://github.com/$repo/releases/latest/download"; else base="https://github.com/$repo/releases/download/$version"; fi
curl -fL --retry 3 --max-time 300 "$base/$asset" -o "$temp/$asset"
curl -fL --retry 3 --max-time 60 "$base/SHA256SUMS" -o "$temp/SHA256SUMS"
cd "$temp"
awk -v asset="$asset" '$2 == asset { print }' SHA256SUMS > selected-checksum
if [[ ! -s selected-checksum ]]; then echo "Release checksum is missing." >&2; exit 1; fi
shasum -a 256 -c selected-checksum
tar -xzf "$asset"
[[ -x speech/whisper-cli ]] || { echo "This release has no bundled speech runtime. Install the latest release." >&2; exit 1; }
dir="$HOME/.fastcontrol"
mkdir -p "$dir" "$HOME/Library/LaunchAgents"
chmod 700 "$dir"
install -m 755 fastcontrol "$dir/fastcontrol.next"
mv "$dir/fastcontrol.next" "$dir/fastcontrol"
mkdir -p "$dir/speech" "$dir/models"
install -m 755 speech/whisper-cli "$dir/speech/whisper-cli.next"
"$dir/speech/whisper-cli.next" --version >/dev/null
mv "$dir/speech/whisper-cli.next" "$dir/speech/whisper-cli"
cp speech/Whisper-LICENSE.txt "$dir/speech/"
cp -R licenses "$dir/"
status="$("$dir/fastcontrol" -speech-status)"
if [[ "$skip_speech" != 1 && "$status" != *'"available":true'* ]] && [[ -t 1 && -r /dev/tty ]]; then
  printf 'Install the speech model now? [Y/n] ' > /dev/tty
  read -r choice < /dev/tty || choice=y
  case "$choice" in n|N|no|NO) skip_speech=1;; esac
fi
if [[ "$skip_speech" == 1 ]]; then
  echo "Speech model setup skipped. You can install it from the Android microphone button."
else
  bash speech/setup.sh "$dir"
fi
if [[ ! -f "$HOME/Library/LaunchAgents/com.fastcontrol.plist" ]]; then
  sed -e "s#__BIN__#$dir/fastcontrol#" -e "s#__HOME__#$HOME#g" com.fastcontrol.plist > "$HOME/Library/LaunchAgents/com.fastcontrol.plist"
fi
label="gui/$(id -u)/com.fastcontrol"
if launchctl print "$label" >/dev/null 2>&1; then
  launchctl kickstart -k "$label"
else
  launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.fastcontrol.plist"
fi
echo "Installed fastcontrol. Pairing links are in $dir/relay.log."
