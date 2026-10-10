#!/bin/bash
set -euo pipefail
dir="${1:?Pass The Installed fastcontrol Directory}"
echo "Preparing Local Speech Transcription (57 MiB If a Model Is Needed)."
"$dir/fastcontrol" -install-speech
