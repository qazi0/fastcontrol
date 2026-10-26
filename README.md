# fastcontrol

Instant access to your existing Codex and Claude Code sessions from Android. fastcontrol connects directly to your computer over Wi-Fi or Tailscale, so you can open a running chat, follow its live output and control it immediately.

## Features

- Browse projects and chats across multiple computers. Send messages, stop tasks, answer questions and approve tool requests.
- See desktop groups and child chats. Rename chats or fork them with their conversation history.
- Choose the model, reasoning effort and permission mode for a chat.
- Check subscription usage limits and reset times. View each computer's local token counts and estimated API costs for today and the last 30 days.
- Record voice messages and have your computer transcribe them into an editable draft.
- Attach images, PDFs and other local files to a chat. View chat images full-screen, zoom and download them.
- Play generated audio in chat and download linked files to Android.
- Read Markdown files from the connected computer inside the app.
- Follow Codex and Claude subagent transcripts, Claude workflow workers and background command output in a read-only view.
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

Download the [versioned Android APK from the latest release](https://github.com/qazi0/fastcontrol/releases/latest), named `fastcontrol_android_v<VERSION>.apk`. Android 8 or newer is required.

After the first install, the app checks GitHub for updates. You can also choose **Check for updates** in Computers. Download and install from the app, or defer until later. Android asks you to approve installation.

Open the pairing link from `~/.fastcontrol/relay.log` on Mac or Linux, or `%USERPROFILE%\.fastcontrol\relay.log` on Windows. You can also choose Add Computer in Android and enter the computer's address and token. The relay uses port 8787. Allow the relay through your firewall when prompted. Use Tailscale to connect outside your LAN.

Repeat the computer installation to add another host. Each computer keeps its own coding sessions and credentials.

## Encrypted connections

The installer enables TLS for network connections. Android 0.2.10 or newer pairs with the computer's certificate using the `fastcontrol://pair` link in its relay log. Copy that complete link into Add Computer, or scan the QR shown when starting the relay without `-no-qr`. You do not need a domain or a public certificate.

The same pairing works over LAN and Tailscale. Connections verify the paired public key, certificate dates and the target address. A secure pairing never falls back to HTTP. Keep `~/.fastcontrol/tls/key.pem` private and backed up; replacing it requires pairing devices again. Certificate renewal retains the key.

After updating an older installation, install the new APK and pair again with the secure link. For manual relay startup, add `-tls`. Loopback remains HTTP for local tools and SSH tunnels. Browsers use their system certificate trust; use an SSH tunnel if you do not want to install the relay's certificate on that computer.

## Use From Another Computer

Open the target computer's relay address in a browser and pair with its token. The same interface controls its Codex and Claude Code sessions. Add other hosts through Computers.

If your browser blocks LAN addresses, forward the relay through SSH:

```sh
ssh -N -L 127.0.0.1:8789:127.0.0.1:8787 you@other-mac
```

Open `http://127.0.0.1:8789/` and pair with the target computer's token. Commands and sessions run on that computer.

## Usage and Speech

Subscription limits come from the account signed in on the selected computer. Computers sharing an account share its quota. Token and cost totals come from that computer's local logs. Dollar amounts estimate the API value of those tokens, including cached usage; they are separate from subscription charges. Unpriced models are identified rather than assigned a guessed cost.

Audio is sent to the computer selected for the chat, transcribed locally and returned as a draft. No speech model runs on the phone. Mac uploads use 16 kHz mono AAC at 32 kbps. Linux and Windows support AAC when FFmpeg is installed and use WAV otherwise. Android recording continues while you switch apps. Return to stop recording, or use the recording notification to transcribe or cancel. Recordings continue until you stop them. Failed transcriptions can be retried using the saved recording. Audio is removed after successful transcription or when you discard it.

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
