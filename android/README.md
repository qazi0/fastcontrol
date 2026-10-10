# fastcontrol for Android

The Android app bundles `../web`, records 16 kHz mono PCM audio, watches paired Macs for notifications and handles `fastcontrol://pair` links. The application ID is `net.siraj.fastcontrol`.

## Build

Install Java 17 and Android SDK 36. Populate Gradle dependencies on a new machine with `cd android && ./gradlew assembleDebug`; subsequent builds can run offline with `make android`.

`make android-install SERIAL=DEVICE` installs the debug APK. For distribution, configure the signing key as described in the root README and run `make android-release`.

## Data and Permissions

Pairing data stays in app-private storage. Recordings stay in cache until the user presses Transcribe, then go to an available paired Mac. Temporary recordings are deleted after completion or cancellation. The returned text goes into the composer for review.

The app requests microphone and notification permissions when the associated feature is used. Recording stops when the app leaves the foreground. Debug builds enable WebView debugging; release builds do not.
