#!/bin/sh
# Build and package Habakkuk Desktop for one platform, into apps/desktop/dist.
#
#   apps/desktop/scripts/package.sh darwin    # Habakkuk.app (universal), .zip and .dmg; macOS only
#   apps/desktop/scripts/package.sh linux     # tar.gz with the app, habakkuk, a .desktop file and icon
#   apps/desktop/scripts/package.sh windows   # habakkuk-desktop.exe; run in Git Bash on Windows
#
# VERSION (default: dev) goes into the binaries and file names. The builds
# are unsigned: macOS gets an ad-hoc signature only, Windows none (see
# docs/interfaces/desktop-app.md). Wails v3 apps are plain Go programs, so this uses go
# build directly rather than the wails3 Taskfile scaffolding.
set -eu

target=${1:?usage: package.sh darwin|linux|windows}
cd "$(dirname "$0")/.." || exit 1
desktop=$(pwd)
root=$(cd ../.. && pwd)
version=${VERSION:-dev}
dist=$desktop/dist
tmp=$dist/tmp
ldflags="-s -w -X main.version=$version"
name="Habakkuk-Desktop_${version}"

rm -rf "$tmp"
mkdir -p "$tmp"

# habakkuk_bin OS ARCH OUT builds the habakkuk binary (static, no CGO) to bundle.
habakkuk_bin() {
	(cd "$root" && CGO_ENABLED=0 GOOS=$1 GOARCH=$2 go build -trimpath -ldflags "$ldflags" -o "$3" ./cmd/habakkuk)
}

case $target in
darwin)
	export CGO_ENABLED=1 MACOSX_DEPLOYMENT_TARGET=11.0
	for arch in arm64 amd64; do
		GOARCH=$arch go build -trimpath -ldflags "$ldflags" -o "$tmp/habakkuk-desktop-$arch" .
		habakkuk_bin darwin "$arch" "$tmp/habakkuk-$arch"
	done
	app=$dist/Habakkuk.app
	rm -rf "$app"
	mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
	lipo -create -output "$app/Contents/MacOS/habakkuk-desktop" "$tmp/habakkuk-desktop-arm64" "$tmp/habakkuk-desktop-amd64"
	lipo -create -output "$app/Contents/MacOS/habakkuk" "$tmp/habakkuk-arm64" "$tmp/habakkuk-amd64"
	sed "s/@VERSION@/${version#v}/g" packaging/darwin/Info.plist >"$app/Contents/Info.plist"
	go run ./cmd/icons -iconset "$tmp/icon.iconset"
	iconutil -c icns -o "$app/Contents/Resources/icon.icns" "$tmp/icon.iconset"
	# Ad-hoc: lets it run on Apple silicon and gives notifications a stable
	# identity. Not a Developer ID signature, so Gatekeeper still asks.
	codesign --force --deep --sign - "$app"
	ditto -c -k --keepParent "$app" "$dist/${name}_macos_universal.zip"
	mkdir -p "$tmp/dmg"
	cp -R "$app" "$tmp/dmg/"
	ln -s /Applications "$tmp/dmg/Applications"
	hdiutil create -quiet -volname Habakkuk -srcfolder "$tmp/dmg" -ov -format UDZO "$dist/${name}_macos_universal.dmg"
	;;
linux)
	arch=$(go env GOARCH)
	stage=$tmp/habakkuk-desktop_linux_$arch
	mkdir -p "$stage"
	CGO_ENABLED=1 go build -trimpath -ldflags "$ldflags" -o "$stage/habakkuk-desktop" .
	habakkuk_bin linux "$arch" "$stage/habakkuk"
	go run ./cmd/icons -png 256 "$stage/habakkuk-desktop.png"
	cp packaging/linux/habakkuk-desktop.desktop "$root/LICENSE" "$stage/"
	tar -C "$tmp" -czf "$dist/${name}_linux_$arch.tar.gz" "habakkuk-desktop_linux_$arch"
	;;
windows)
	# The habakkuk binary does not build for Windows yet, so the app only
	# connects to remote Habakkuks there (or a habakkuk.exe you point it at).
	# go build links the .syso next to main.go: the icon, the manifest
	# (packaging/windows) and the version info.
	syso=rsrc_windows_amd64.syso
	go run ./cmd/icons -syso "$syso" -arch amd64 -version "$version"
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$ldflags -H windowsgui" -o "$dist/${name}_windows_amd64.exe" .
	rm -f "$syso"
	;;
*)
	echo "unknown target $target" >&2
	exit 2
	;;
esac
rm -rf "$tmp"
ls -l "$dist"
