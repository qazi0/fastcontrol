#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
case "$(uname -m)" in aarch64) arch=arm64;; x86_64) arch=amd64;; *) exit 1;; esac
package_directory="$PWD/dist"
test_home="$(mktemp -d)"
trap 'if [[ -f "$test_home/.fastcontrol/relay.pid" ]]; then kill "$(cat "$test_home/.fastcontrol/relay.pid")" 2>/dev/null || true; fi; rm -rf "$test_home"' EXIT
export HOME="$test_home"
mkdir -p "$HOME/.local/bin"
# The install check needs a provider present, without credentials or inference.
printf '#!/bin/sh\nexit 0\n' > "$HOME/.local/bin/claude"
chmod 755 "$HOME/.local/bin/claude"
FASTCONTROL_PACKAGE_DIRECTORY="$package_directory" bash install.sh --skip-speech
"$HOME/.fastcontrol/fastcontrol" -speech-status | python3 -c 'import json,sys; assert not json.load(sys.stdin)["available"]'
"$HOME/.fastcontrol/fastcontrol" -install-speech
"$HOME/.fastcontrol/fastcontrol" -speech-status | python3 -c 'import json,sys; s=json.load(sys.stdin); assert s["available"] and s["engine"]=="whispercpp"'
for i in {1..20}; do [[ -s "$HOME/.fastcontrol/token" ]] && break; sleep 0.2; done
curl -fsS --max-time 10 -o /dev/null "http://127.0.0.1:8787/auth?t=$(cat "$HOME/.fastcontrol/token")"
[[ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8787/auth)" == 401 ]]
curl -fsS http://127.0.0.1:8787/usage.js -o "$test_home/usage.js"
[[ -s "$test_home/usage.js" ]]
sed "s#%h#$test_home#g" contrib/fastcontrol.service > "$test_home/fastcontrol.service"
systemd-analyze verify "$test_home/fastcontrol.service"
echo "Installed Linux Package And Speech Setup Passed"
