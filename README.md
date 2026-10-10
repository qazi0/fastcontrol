# fastcontrol

Control your Mac's coding sessions from an Android phone over Wi-Fi or Tailscale. The relay runs on your Mac and keeps transcripts and credentials there.

Choose Codex or Claude Code when you open the app. Each provider shows its own projects, sessions, model catalogue and permission controls.

## Features

- Several Macs, projects, pinned chats, live messages, steering, Stop, approvals, questions, model and effort controls.
- Text reveal and a Thinking indicator, quick screen transitions, and a context ring with activity details.
- Android voice recording with a waveform and transcription spinner. Audio goes to an available paired Mac running a local MacWhisper model. The transcript becomes an editable draft.
- Inline attachments, generated images and images opened by tools. Full-screen viewing supports zoom, pan and native image downloading.
- Background notifications for finished turns and requests needing attention.

## Install On a Mac

This repository is private. You need repository access and an authenticated GitHub CLI (`gh auth login`). Install the latest release:

```sh
bash -c 'set -euo pipefail; gh api repos/qazi0/fastcontrol/contents/install.sh --jq .content | base64 -D | bash'
```

The installer downloads the Mac build, verifies its SHA-256 checksum, sets up local speech transcription and installs a login service. It requires macOS 13.3 or newer. It supports Apple Silicon Macs.

The relay uses port 8787 and stores its token, binary and logs in `~/.fastcontrol/`. Pairing links appear in `~/.fastcontrol/relay.log`. Allow incoming connections if your firewall prompts.

To skip speech setup from the one-line installer:

```sh
bash -c 'set -euo pipefail; gh api repos/qazi0/fastcontrol/contents/install.sh --jq .content | base64 -D | bash -s -- --skip-speech'
```

## Android App

Download `fastcontrol_android.apk` from [Releases](https://github.com/qazi0/fastcontrol/releases). Install it, open a pairing link, or use Add Mac. Private release downloads require GitHub repository access.

Android 8 or newer is required. Voice recording requests microphone permission; notifications request notification permission on newer Android versions.

## Build From Source

```sh
gh repo clone qazi0/fastcontrol
cd fastcontrol
make test
make
./fastcontrol -no-qr
```

Requirements: macOS, Go 1.23 or newer, and either Codex desktop or an authenticated Claude Code installation. Codex live control requires the desktop app and its local IPC socket and state database. The protocol is private and checked against the installed desktop bundle at startup. App updates can require protocol changes.

```sh
make install                          # Login service
make android                          # Debug APK
make android-install SERIAL=DEVICE    # Install on a connected phone
make android-release                  # Signed APK, requires signing configuration
make package                          # Mac release archives and checksums
```

The Android build uses Java 17, Android SDK 36 and the Gradle wrapper. The Makefile uses offline Gradle resolution, so populate the dependency cache with a normal Gradle build first on a new machine.

## Release Signing

Create `android/signing.properties` locally. It is excluded from Git:

```properties
storeFile=/absolute/path/to/release.jks
storePassword=YOUR_PASSWORD
keyAlias=fastcontrol
keyPassword=YOUR_PASSWORD
```

Keep the keystore and its password private and backed up. Subsequent APK releases must use the same key.

Packaging the speech runtime requires CMake and the macOS developer tools on the build machine. End users install prebuilt binaries. Run `make android-release` and `make package`, then upload the contents of `dist/` to a GitHub release. The archives contain the relay and its LaunchAgent template. Never commit tokens, transcripts, recordings, signing keys or machine-specific settings.

## Architecture

The Go server reads the Codex state database and transcript files and connects to the desktop IPC socket for live control. For Claude Code it reads native JSONL sessions and uses the installed CLI with bidirectional streaming, tool approvals and interrupt controls. The same web UI runs in a browser and inside the Android WebView. Android adds microphone recording, notifications, pairing and device integration.

The installer asks whether to set up speech now. You can skip it with `--skip-speech` or `FASTCONTROL_SKIP_SPEECH=1`. The installer reuses a supported installed MacWhisper model, preferring Parakeet v3. When none is available, it automatically downloads a multilingual Whisper Base Q5 model (57 MiB) and uses the bundled whisper.cpp runtime. The runtime and model work without MacWhisper, Homebrew, Python, a paid speech app or an API key. Source archives and model downloads are pinned and verified with SHA-256 checksums. Recordings are transcribed locally on the Mac. The relay accepts bounded mono 16 kHz PCM WAV recordings and removes temporary audio after transcription. Voice uses the Mac selected for the chat.

Speech setup needs internet access for the first model download. The installer validates the fallback runtime and prepares its model before starting the relay. Existing working models are kept, and subsequent installations skip the model download. If setup was skipped, tap the Android microphone, then Install On Mac. The selected Mac downloads and prepares the model, while Android shows progress. You can close the prompt while installation continues. Run the installer again to repair a missing or corrupted fallback model. `./fastcontrol -speech-status` reports the chosen engine and model. Set `FASTCONTROL_SPEECH_ENGINE=whispercpp` when invoking the relay to select the bundled engine explicitly.

The relay authenticates API requests with a random token and binds to loopback, Wi-Fi and Tailscale addresses. Use Tailscale when connecting outside a trusted LAN. Tokens grant control of sessions running with the permissions selected on the Mac.

## Claude Code

Install and sign in to Claude Code on each Mac. The relay finds the normal user and Homebrew binary locations. It lists sessions from `~/.claude/projects/`, follows transcript changes and resumes sessions through the CLI. Model choices come from the installed CLI, and the chat shows the resolved model name.

A session already managed by a Mac process opens for viewing. Take Over explicitly stops that process before resuming it under the relay. Follow-ups queue while Claude is responding. Stop clears queued follow-ups and interrupts the current turn. Settings can change after the turn finishes. Archive hides a session in fastcontrol without deleting its native transcript.

Claude Code CLI changes can require adapter updates. Live verification used Haiku 5.5 at Low effort, including approval, streaming, queued turns and sending after Stop.

## Add Another Mac

Run the installer on the second Mac, then open Macs in Android and choose Add Mac. Enter its LAN or Tailscale URL and pairing token from its private relay log. Each Mac keeps its own sessions and credentials. You can switch Macs or browse their combined lists.

## Verification

Go tests cover transcript conversion, patches, paging, voice input validation and image serving. Both Mac installations and the Galaxy A73 have been exercised with real desktop sessions. Recent phone transcription checks returned drafts in 1.08 to 1.38 seconds over Wi-Fi for a short recorded test phrase. These are measured examples, not latency guarantees.

Ubuntu fonts are distributed under the licence in `licenses/Ubuntu-Font-Licence.txt`. The bundled [whisper.cpp runtime](https://github.com/ggml-org/whisper.cpp) and [OpenAI Whisper model](https://github.com/openai/whisper) use MIT licences included in the release.
