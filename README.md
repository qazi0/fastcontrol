# fastcontrol

Instant access to your existing Codex and Claude Code sessions from Android. fastcontrol connects directly to your computer over Wi-Fi or Tailscale, so you can open a running chat, follow its live output and control it immediately.

## Features

- Browse projects and chats across multiple computers. Send messages, stop tasks, answer questions and approve tool requests.
- Choose the model, reasoning effort and permission mode for a chat.
- Check subscription usage limits and reset times. View each computer's local token counts and estimated API costs for today and the last 30 days.
- Record voice messages and have your computer transcribe them into an editable draft.
- View chat images full-screen, zoom and download them.
- Play generated audio in chat and download linked files to Android.
- Open subagent transcripts and follow their progress in a read-only view.
- Receive notifications when tasks finish or need your attention.

## Install on a Mac

Requires Apple Silicon and macOS 13.3 or newer. Install Codex desktop or Claude Code and sign in, then run:

```sh
curl -fsSL https://github.com/qazi0/fastcontrol/releases/latest/download/install.sh | bash
```

The installer downloads the relay, verifies its checksum and starts it at login. It also sets up speech transcription. The default model is multilingual Whisper Base Q5: **59,707,625 bytes (56.94 MiB)**. If a supported local MacWhisper model is already installed, fastcontrol reuses it, preferring Parakeet v3.

To skip the model download:

```sh
curl -fsSL https://github.com/qazi0/fastcontrol/releases/latest/download/install.sh | bash -s -- --skip-speech
```

You can install the model later from the Android microphone button.

## Install on Linux

Supports ARM64 and x86_64 Linux with glibc 2.35 or newer, including Ubuntu 22.04 and later. Install the Codex CLI or Claude Code and sign in, then run the same installer:

```sh
curl -fsSL https://github.com/qazi0/fastcontrol/releases/latest/download/install.sh | bash
```

The installer verifies the package checksum, installs local Whisper transcription and starts a systemd user service. On systems without a user service manager, it starts the relay in the background. Use `--skip-speech` to defer the 56.94 MiB model download. Install FFmpeg for compressed voice uploads.

Codex chats run through the local app-server, with streaming, follow-up messages, stop controls and approval prompts. An existing shared Codex daemon is reused when available. Claude Code uses its installed CLI. A desktop app is not required.

Manage the service with `systemctl --user restart fastcontrol` or `systemctl --user stop fastcontrol`. For a headless host that must stay online after logout, enable user lingering with `loginctl enable-linger "$USER"`.

## Install on Windows

Requires Windows 11 ARM64 and a signed-in Codex desktop or native Claude Code installation. Run in PowerShell:

```powershell
irm https://github.com/qazi0/fastcontrol/releases/latest/download/install.ps1 | iex
```

The installer sets up the relay, a login task and local Whisper transcription. To skip the model download, set `$env:FASTCONTROL_SKIP_SPEECH = '1'` before running it.

Windows installation and speech setup are tested on ARM64. Live control of signed-in Windows coding sessions still needs verification.

## Install the Android App

Download [fastcontrol_android.apk from the latest release](https://github.com/qazi0/fastcontrol/releases/latest). Android 8 or newer is required.

Open the pairing link from `~/.fastcontrol/relay.log` on Mac or Linux, or `%USERPROFILE%\.fastcontrol\relay.log` on Windows. You can also choose Add Computer in Android and enter the computer's address and token. The relay uses port 8787. Allow the relay through your firewall when prompted. Use Tailscale to connect outside your LAN.

Repeat the computer installation to add another host. Each computer keeps its own coding sessions and credentials.

## Usage and Speech

Subscription limits come from the account signed in on the selected computer. Computers sharing an account share its quota. Token and cost totals come from that computer's local logs. Dollar amounts estimate the API value of those tokens, including cached usage; they are separate from subscription charges. Unpriced models are identified rather than assigned a guessed cost.

Audio is sent to the computer selected for the chat, transcribed locally and returned as a draft. No speech model runs on the phone. Mac uploads use 16 kHz mono AAC at 32 kbps. Linux and Windows support AAC when FFmpeg is installed and use WAV otherwise. Recordings are limited to two minutes and temporary audio is removed after transcription.

## Build From Source

Requires Go 1.23 or newer:

```sh
git clone https://github.com/qazi0/fastcontrol.git
cd fastcontrol
make test
make
./fastcontrol -no-qr
```

Mac commands:

```sh
make install          # Start the relay at login
make android          # Debug APK
make android-release  # Signed APK
make package          # Mac archive and checksums
```

Android builds use Java 17 and Android SDK 36. The Makefile builds Gradle offline; populate the dependency cache with a normal Gradle build on a new machine. Linux packaging uses `scripts/package-linux.sh`, GCC and CMake. Windows packaging uses `scripts/build-windows.ps1`, CMake and Visual Studio with Clang and ARM64 C++ tools.

Create `android/signing.properties` locally for signed APKs:

```properties
storeFile=/absolute/path/to/release.jks
storePassword=YOUR_PASSWORD
keyAlias=fastcontrol
keyPassword=YOUR_PASSWORD
```

Keep the signing key, tokens and recordings out of Git. A pairing token grants control of the sessions available through its relay.

On Mac and Windows, the Codex adapter uses the desktop app's local IPC protocol, which can change with app updates. Linux uses the CLI app-server protocol. Claude Code uses native transcripts and the installed CLI. External Claude sessions open for viewing; Take Over stops the existing process before resuming it through fastcontrol. Archive preserves the native transcript.

[Release downloads](https://github.com/qazi0/fastcontrol/releases) include the relay, speech runtime and Android APK. Dependency licences are in `licenses/`. Ubuntu fonts, whisper.cpp, Whisper and the provider marks retain their respective licences and trademarks.
