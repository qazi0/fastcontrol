# The default Go caches on this Mac are root-owned, so builds use user-owned ones.
export GOPATH ?= $(HOME)/.cache/gopath
export GOMODCACHE ?= $(HOME)/.cache/gopath/pkg/mod
export GOCACHE ?= $(HOME)/.cache/go-build
export GOFLAGS ?= -modcacherw

fastcontrol: $(wildcard *.go) $(wildcard web/*)
	go build -trimpath -ldflags "-s -w" -o fastcontrol .

run: fastcontrol
	./fastcontrol

test:
	go vet ./... && go test ./...

.PHONY: run test

# Installs a per-user LaunchAgent so the relay starts at login and restarts on crash.
install: fastcontrol
	mkdir -p $(HOME)/.fastcontrol $(HOME)/Library/LaunchAgents
	cp fastcontrol $(HOME)/.fastcontrol/fastcontrol.next
	mv $(HOME)/.fastcontrol/fastcontrol.next $(HOME)/.fastcontrol/fastcontrol
	sed -e "s#__BIN__#$(HOME)/.fastcontrol/fastcontrol#" -e "s#__HOME__#$(HOME)#g" contrib/com.fastcontrol.plist > $(HOME)/Library/LaunchAgents/com.fastcontrol.plist
	launchctl bootout gui/$$(id -u)/com.fastcontrol 2>/dev/null || true
	launchctl bootstrap gui/$$(id -u) $(HOME)/Library/LaunchAgents/com.fastcontrol.plist
	@echo "Pairing link: http://<this-mac-ip>:8787/#t=$$(cat $(HOME)/.fastcontrol/token 2>/dev/null)"

uninstall:
	launchctl bootout gui/$$(id -u)/com.fastcontrol 2>/dev/null || true
	rm -f $(HOME)/Library/LaunchAgents/com.fastcontrol.plist

.PHONY: install uninstall

# Android shell app (android/). Builds offline from the local Gradle cache and bundles web/.
ANDROID_APK := android/app/build/outputs/apk/debug/app-debug.apk
ADB ?= $(HOME)/Library/Android/sdk/platform-tools/adb

android:
	cd android && ANDROID_HOME=$(HOME)/Library/Android/sdk ./gradlew --offline assembleDebug
	@echo "APK: $(CURDIR)/$(ANDROID_APK)"

android-install: android
	$(ADB) $(if $(SERIAL),-s $(SERIAL)) install -r $(ANDROID_APK)

.PHONY: android android-install

android-release:
	cd android && ANDROID_HOME=$(HOME)/Library/Android/sdk ./gradlew --offline assembleRelease

package: fastcontrol
	./scripts/package-release.sh

.PHONY: android-release package
