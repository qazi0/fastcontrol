#!/bin/bash
set -euo pipefail

repo="qazi0/fastcontrol"
version="${FASTCONTROL_VERSION:-latest}"
skip_speech="${FASTCONTROL_SKIP_SPEECH:-0}"
case "${1:-}" in --skip-speech) skip_speech=1;; "") ;; *) echo "Usage: install.sh [--skip-speech]" >&2; exit 1;; esac
platform="$(uname -s)"
case "$platform" in
Darwin)
mac_version="$(sw_vers -productVersion)"
mac_major="${mac_version%%.*}"
mac_minor="${mac_version#*.}"; mac_minor="${mac_minor%%.*}"
if (( mac_major < 13 || (mac_major == 13 && mac_minor < 3) )); then echo "fastcontrol requires macOS 13.3 or newer." >&2; exit 1; fi
[[ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" == 1 ]] || { echo "fastcontrol Supports Apple Silicon Macs Only" >&2; exit 1; }
arch=arm64
os=darwin
checksums=SHA256SUMS
;;
Linux)
case "$(uname -m)" in aarch64|arm64) arch=arm64;; x86_64) arch=amd64;; *) echo "fastcontrol Supports Linux ARM64 And x86_64" >&2; exit 1;; esac
os=linux
checksums="SHA256SUMS_linux_$arch"
command -v curl >/dev/null || { echo "Install curl First" >&2; exit 1; }
command -v sha256sum >/dev/null || { echo "Install coreutils First" >&2; exit 1; }
;;
*) echo "fastcontrol Requires macOS Or Linux" >&2; exit 1;;
esac
temp="$(mktemp -d)"
trap 'rm -rf "$temp"' EXIT
asset="fastcontrol_${os}_${arch}.tar.gz"
if [[ "$version" == latest ]]; then base="https://github.com/$repo/releases/latest/download"; else base="https://github.com/$repo/releases/download/$version"; fi
if [[ -n "${FASTCONTROL_PACKAGE_DIRECTORY:-}" ]]; then
 cp "$FASTCONTROL_PACKAGE_DIRECTORY/$asset" "$temp/$asset"
 cp "$FASTCONTROL_PACKAGE_DIRECTORY/$checksums" "$temp/SHA256SUMS"
else
 curl -fL --retry 3 --max-time 300 "$base/$asset" -o "$temp/$asset"
 curl -fL --retry 3 --max-time 60 "$base/$checksums" -o "$temp/SHA256SUMS"
fi
cd "$temp"
awk -v asset="$asset" '$2 == asset { print }' SHA256SUMS > selected-checksum
if [[ ! -s selected-checksum ]]; then echo "Release checksum is missing." >&2; exit 1; fi
if [[ "$platform" == Linux ]]; then sha256sum -c selected-checksum; else shasum -a 256 -c selected-checksum; fi
tar -xzf "$asset"
[[ -x speech/whisper-cli ]] || { echo "This release has no bundled speech runtime. Install the latest release." >&2; exit 1; }
dir="$HOME/.fastcontrol"
mkdir -p "$dir"
if [[ "$platform" == Darwin ]]; then mkdir -p "$HOME/Library/LaunchAgents"; fi
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
if [[ "$platform" == Linux ]]; then
 if command -v systemctl >/dev/null && systemctl --user show-environment >/dev/null 2>&1; then
  mkdir -p "$HOME/.config/systemd/user"
  [[ -f "$HOME/.config/systemd/user/fastcontrol.service" ]] || cp fastcontrol.service "$HOME/.config/systemd/user/fastcontrol.service"
  systemctl --user import-environment PATH
  systemctl --user daemon-reload
  systemctl --user enable fastcontrol.service
  systemctl --user restart fastcontrol.service
  systemctl --user is-active --quiet fastcontrol.service
 else
  pidfile="$dir/relay.pid"
  if [[ -f "$pidfile" ]]; then
   read -r previous_pid < "$pidfile" || true
   if [[ "${previous_pid:-}" =~ ^[0-9]+$ ]]; then
    previous_binary="$(readlink "/proc/$previous_pid/exe" 2>/dev/null || true)"
    if [[ "$previous_binary" == "$dir/fastcontrol" || "$previous_binary" == "$dir/fastcontrol (deleted)" ]]; then kill "$previous_pid"; fi
   fi
  fi
  nohup "$dir/fastcontrol" -no-qr >> "$dir/relay.log" 2>&1 < /dev/null &
  relay_pid=$!
  echo "$relay_pid" > "$pidfile"
  sleep 1
  kill -0 "$relay_pid" 2>/dev/null || { echo "Relay Did Not Start. See $dir/relay.log" >&2; exit 1; }
  echo "Started In The Background. A systemd User Service Is Needed For Automatic Login Startup."
 fi
 echo "Installed fastcontrol. Pairing links are in $dir/relay.log."
 exit 0
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
