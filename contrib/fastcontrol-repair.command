#!/bin/bash
# Local recovery never replaces pairing credentials or the computer's TLS key.
set -u
dir="$HOME/.fastcontrol"
label="gui/$(id -u)/com.fastcontrol"
check_only=0
[[ "${1:-}" != --check ]] || check_only=1
echo "fastcontrol connection check"
echo
tailscale=/Applications/Tailscale.app/Contents/MacOS/Tailscale
if [[ -x "$tailscale" ]]; then
  state="$("$tailscale" status --json 2>/dev/null | /usr/bin/plutil -extract BackendState raw -o - - 2>/dev/null)"
  echo "Tailscale: ${state:-could not read status}"
  if [[ "$state" == Stopped && "$check_only" == 0 ]]; then
    echo "Reconnecting Tailscale..."
    "$tailscale" up > /dev/null 2>&1 &
    task=$!
    for ((i=0;i<20;i++)); do kill -0 "$task" 2>/dev/null || break; sleep 1; done
    if kill -0 "$task" 2>/dev/null; then kill "$task"; echo "Open Tailscale to finish connecting."; fi
    wait "$task" 2>/dev/null || true
  elif [[ "$state" != Running && "$check_only" == 0 ]]; then
    echo "Open Tailscale and check its connection or sign-in."
    /usr/bin/open -a Tailscale
  fi
else
  echo "Tailscale is not installed. LAN connections can still work."
fi
if [[ "$check_only" == 0 ]]; then
  recording=0
  for path in "$dir"/voice/recording-*; do [[ ! -e "$path" ]] || recording=1; done
  if [[ "$recording" == 1 ]]; then
    echo "A voice upload is in progress. Relay restart skipped. Try again after it finishes."
  elif [[ ! -f "$HOME/Library/LaunchAgents/com.fastcontrol.plist" ]]; then
    echo "The login service is missing. Run the fastcontrol installer again."
  else
    if ! launchctl print "$label" > /dev/null 2>&1; then
      launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.fastcontrol.plist"
    fi
    if launchctl kickstart -k "$label"; then echo "Relay restarted."; else echo "Could not restart the relay. Run this from your signed-in Mac desktop."; fi
  fi
fi
ready=0
for ((i=0;i<10;i++)); do
  if /usr/bin/curl --noproxy '*' -fsS --max-time 2 http://127.0.0.1:8787/ -o /dev/null 2>/dev/null; then ready=1; break; fi
  [[ "$check_only" == 0 ]] || break
  sleep 1
done
echo
if [[ "$ready" == 1 ]]; then
  echo "Relay: responding"
  echo "Listening addresses:"
  /usr/sbin/lsof -nP -a -u "$(id -un)" -c fastcontrol -iTCP:8787 -sTCP:LISTEN 2>/dev/null | /usr/bin/awk 'NR>1 {print "  " $9}'
  echo "If your phone still cannot connect, allow fastcontrol in your firewall and connect Tailscale on the phone."
else
  echo "Relay: not responding on port 8787. Check ~/.fastcontrol/relay.log locally."
  echo "If you changed the port, check that port instead."
fi
echo
if [[ -t 0 && "${1:-}" != --no-wait && "$check_only" == 0 ]]; then
  read -r -p "Press Return to close. " ignored
fi
[[ "$ready" == 1 ]]
