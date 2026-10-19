# fastcontrol for Android

The Android app bundles `../web`, records voice for host transcription, plays generated audio and downloads linked files. It watches paired computers for notifications and handles `fastcontrol://pair` links. The application ID is `net.siraj.fastcontrol`.

## Build

Install Java 17 and Android SDK 36. Populate Gradle dependencies on a new machine with `cd android && ./gradlew assembleDebug`; subsequent builds can run offline with `make android`.

`make android-install SERIAL=DEVICE` installs the debug APK. For distribution, configure the signing key as described in the root README and run `make android-release`.

`make package` copies the signed APK to `dist/fastcontrol_android_v<VERSION>.apk` for publication.

## Data and Permissions

Pairing data stays in app-private storage. Tap the microphone to record and tap it again to transcribe on the selected computer. Audio uses 16 kHz mono AAC at 32 kbps when the host supports it. Temporary recordings are deleted after completion or cancellation. The returned text goes into the composer for review.

Generated audio streams from the paired relay with authenticated byte-range requests. Linked files of any type save to Downloads/fastcontrol on Android 10 and newer; Android 8 and 9 use the system save picker. Downloads stream to storage without a file-size cap. Subagent transcripts open in a read-only view without changing the parent draft.

The app requests microphone and notification permissions when the associated feature is used. Recording stops when the app leaves the foreground. Debug builds enable WebView debugging; release builds do not.

Attachments use Android's file picker. Photos are prepared on the phone; PDFs, text and other documents upload unchanged to the selected computer. Each attachment can be up to 25 MB. Images are passed as image inputs, while other files are referenced by their local path so the agent can read them with its available tools.
