#!/bin/bash
set -euo pipefail

dir="${1:?Pass The Installed fastcontrol Directory}"
temp="$(mktemp -d)"
trap 'rm -rf "$temp"' EXIT
mkdir -p "$dir/models"
status="$("$dir/fastcontrol" -speech-status)"
if [[ "$status" != *'"available":true'* ]]; then
  echo "Downloading the local multilingual Whisper model (57 MiB)."
  model_checksum=422f1ae452ade6f30a004d7e5c6a43195e4433bc370bf23fac9cc591f01a8898
  model_url="https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-base-q5_1.bin"
  model_temp="$(mktemp "$temp/model.XXXXXX")"
  curl -fL --retry 3 --max-time 600 "$model_url" -o "$model_temp"
  [[ "$(shasum -a 256 "$model_temp" | awk '{print $1}')" == "$model_checksum" ]] || { echo "Speech Model Checksum Failed" >&2; exit 1; }
  install -m 600 "$model_temp" "$dir/models/ggml-base-q5_1.bin.next"
  mv "$dir/models/ggml-base-q5_1.bin.next" "$dir/models/ggml-base-q5_1.bin"
  status="$("$dir/fastcontrol" -speech-status)"
fi
[[ "$status" == *'"available":true'* ]] || { echo "Local speech setup failed. Run the installer again." >&2; exit 1; }
if [[ "$status" == *'"engine":"whispercpp"'* ]]; then
  echo "Preparing the speech model for first use."
  # A one-second silent WAV checks model loading and warms the GPU shader cache.
  printf 'RIFF\x24\x7d\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80\x3e\x00\x00\x00\x7d\x00\x00\x02\x00\x10\x00data\x00\x7d\x00\x00' > "$temp/check.wav"
  dd if=/dev/zero bs=32000 count=1 2>/dev/null >> "$temp/check.wav"
  "$dir/speech/whisper-cli" --model "$dir/models/ggml-base-q5_1.bin" --file "$temp/check.wav" \
    --language auto --threads 4 --beam-size 1 --best-of 1 --no-timestamps --no-prints \
    --output-txt --output-file "$temp/check" > "$temp/check.log" 2>&1 || { echo "Speech Model Validation Failed" >&2; exit 1; }
  [[ -f "$temp/check.txt" ]] || { echo "Speech Model Validation Failed" >&2; exit 1; }
fi
echo "Local speech transcription is ready."
