# Linux Relay

fastcontrol supports Linux ARM64 and x86_64. Release packages are built on Ubuntu 22.04 and need glibc 2.35 or newer. Alpine and other musl systems need a source build and a compatible whisper.cpp runtime.

## Installation

Install and sign in to the Codex CLI or Claude Code first. Then run:

```sh
curl -fsSL https://raw.githubusercontent.com/qazi0/fastcontrol/main/install.sh | bash
```

Add `bash -s -- --skip-speech` in place of `bash` to defer the model download. The default model is multilingual Whisper Base Q5, 59,707,625 bytes (56.94 MiB). FFmpeg enables compressed voice uploads. WAV works without it.

The installer uses a systemd user service when the user manager is available. Otherwise it starts the relay in the background and records its PID in `~/.fastcontrol/relay.pid`. Pairing links are in `~/.fastcontrol/relay.log`.

```sh
systemctl --user status fastcontrol
systemctl --user restart fastcontrol
systemctl --user stop fastcontrol
```

On a headless machine, `loginctl enable-linger "$USER"` keeps the user manager running after logout. This command may need administrator access. Open TCP port 8787 on the intended LAN or Tailscale interface if the host firewall blocks it.

## Codex Connections

Linux uses the Codex app-server protocol. An existing `~/.codex/app-server-control/app-server-control.sock` is reused. If that socket is absent, the relay starts a private app-server over stdio. It does not start, stop or reconfigure an existing user daemon.

`CODEX_HOME` selects a custom Codex home. Chats are created in the folder selected in Android. New chats, follow-up messages, streaming, model and effort changes, approvals, questions and interruption use the local protocol. Stored chats can be viewed before resuming them. A session held by another Codex process remains subject to Codex's ownership rules.

On Mac and Windows, fastcontrol continues to use desktop IPC for desktop-owned chats. The installed Mac desktop bundle contains `CODEX_APP_SERVER_USE_LOCAL_DAEMON=1`, but that path also checks CLI overrides, plugin configuration and daemon compatibility. Enabling the variable alone does not expose the desktop's current private app-server.

## Verification

Native ARM64 and x86_64 CI runs Go tests, the race detector, packaging, installed relay checks and speech-model setup. Linux packages include the relay, whisper.cpp runtime, service unit and dependency licences. The package check executes both binaries and checks speech-library dependencies before archiving.

A separate local ARM64 Linux test used Codex 0.162.1 to create and continue a chat in a selected directory, reopen stored history and connect through a shared Unix socket. Claude Code 2.1.291 completed a new chat using Haiku 5.5 at Low effort. These tests used isolated session directories.
