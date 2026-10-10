# fastcontrol

Control your Mac's coding sessions from an Android phone over Wi-Fi or Tailscale. The relay runs on your Mac and keeps transcripts and credentials there.

The current baseline supports Codex desktop sessions. Claude Code support is the next implementation checkpoint.

## Features

- Several Macs, projects, pinned chats, live messages, steering, Stop, approvals, questions, model and effort controls.
- Text reveal and a Thinking indicator, quick screen transitions, and a context ring with activity details.
- Android voice recording with a waveform and transcription spinner. Audio goes to an available paired Mac running a local MacWhisper model. The transcript becomes an editable draft.
- Inline attachments, generated images and images opened by tools. Full-screen viewing supports zoom and pan. Native image downloading is still being completed.
- Background notifications for finished turns and requests needing attention.

## Install on a Mac

This repository is private. You need repository access and an authenticated GitHub CLI (`gh auth login`). Once a release is available:

```sh
bash -c 'set -euo pipefail; gh api repos/qazi0/fastcontrol/contents/install.sh --jq .content | base64 -D | bash'
```

The installer downloads the Mac build, verifies its SHA-256 checksum and installs a login service. It works on Apple Silicon and Intel Macs. The first release is being prepared.

The relay uses port 8787 and stores its token, binary and logs in `~/.fastcontrol/`. Pairing links appear in `~/.fastcontrol/relay.log`. Allow incoming connections if your firewall prompts.

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

Requirements: macOS, Go 1.23 or newer, and a running Codex desktop app with its local IPC socket and state database. The protocol is private and checked against the installed desktop bundle at startup. App updates can require protocol changes.

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

Run `make android-release` and `make package`, then upload the contents of `dist/` to a GitHub release. The archives contain the relay and its LaunchAgent template. Never commit tokens, transcripts, recordings, signing keys or machine-specific settings.

## Architecture

The Go server reads the Codex state database and transcript files and connects to the desktop IPC socket for live control. The same web UI runs in a browser and inside the Android WebView. Android adds microphone recording, notifications, pairing and device integration.

Transcription uses the MacWhisper CLI and an explicitly selected installed local model. The current selection prefers Parakeet v3, with installed Whisper models as alternatives. The relay accepts bounded mono 16 kHz PCM WAV recordings and removes temporary audio after transcription. A paired Mac without a speech model can use another paired Mac's available model.

The relay authenticates API requests with a random token and binds to loopback, Wi-Fi and Tailscale addresses. Use Tailscale when connecting outside a trusted LAN. Tokens grant control of sessions running with the permissions selected on the Mac.

## Verification

Go tests cover transcript conversion, patches, paging, voice input validation and image serving. Both Mac installations and the Galaxy A73 have been exercised with real desktop sessions. Recent phone transcription checks returned drafts in 1.08 to 1.38 seconds over Wi-Fi for a short recorded test phrase. These are measured examples, not latency guarantees.

Ubuntu fonts are distributed under the licence in `licenses/Ubuntu-Font-Licence.txt`.
