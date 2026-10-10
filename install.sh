#!/bin/bash
set -euo pipefail

repo="qazi0/fastcontrol"
version="${FASTCONTROL_VERSION:-latest}"
if [[ "$(uname -s)" != Darwin ]]; then echo "fastcontrol requires macOS." >&2; exit 1; fi
if ! command -v gh >/dev/null; then echo "Install GitHub CLI and run gh auth login first." >&2; exit 1; fi
mac_version="$(sw_vers -productVersion)"
mac_major="${mac_version%%.*}"
mac_minor="${mac_version#*.}"; mac_minor="${mac_minor%%.*}"
if (( mac_major < 13 || (mac_major == 13 && mac_minor < 3) )); then echo "fastcontrol requires macOS 13.3 or newer." >&2; exit 1; fi
arch="$(uname -m)"
case "$arch" in arm64|x86_64) ;; *) echo "Unsupported Mac architecture: $arch" >&2; exit 1;; esac
temp="$(mktemp -d)"
trap 'rm -rf "$temp"' EXIT
asset="fastcontrol_darwin_${arch}.tar.gz"
args=(--repo "$repo" --dir "$temp" --pattern "$asset" --pattern SHA256SUMS)
if [[ "$version" == latest ]]; then gh release download "${args[@]}"; else gh release download "$version" "${args[@]}"; fi
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
bash speech/setup.sh "$dir"
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
