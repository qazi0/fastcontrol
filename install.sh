#!/bin/bash
set -euo pipefail

repo="qazi0/fastcontrol"
version="${FASTCONTROL_VERSION:-latest}"
if [[ "$(uname -s)" != Darwin ]]; then echo "fastcontrol requires macOS." >&2; exit 1; fi
if ! command -v gh >/dev/null; then echo "Install GitHub CLI and run gh auth login first." >&2; exit 1; fi
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
dir="$HOME/.fastcontrol"
mkdir -p "$dir" "$HOME/Library/LaunchAgents"
chmod 700 "$dir"
install -m 755 fastcontrol "$dir/fastcontrol.next"
mv "$dir/fastcontrol.next" "$dir/fastcontrol"
sed -e "s#__BIN__#$dir/fastcontrol#" -e "s#__HOME__#$HOME#g" com.fastcontrol.plist > "$HOME/Library/LaunchAgents/com.fastcontrol.plist"
label="gui/$(id -u)/com.fastcontrol"
launchctl bootout "$label" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.fastcontrol.plist"
echo "Installed fastcontrol. Pairing links are in $dir/relay.log."
