package main

import (
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/icon"
)

// bundleID identifies the app: CFBundleIdentifier in Info.plist, the
// Windows manifest's assembly name, and the single-instance lock.
const bundleID = "ai.habakkuk.desktop"

// appIcon is the icon the app shows for itself while running (the Dock on
// macOS, window and taskbar icons elsewhere). On macOS it keeps the margin
// of the macOS icon grid, like the bundle's icon.icns.
func appIcon() []byte {
	if runtime.GOOS == "darwin" {
		return icon.MacApp(512)
	}
	return icon.App(256)
}

// windowBackground is what a window shows before its page paints: the
// dashboard's page colour, cream (light) or ink (dark).
func windowBackground(dark bool) application.RGBA {
	if dark {
		return application.NewRGB(0x0e, 0x11, 0x16)
	}
	return application.NewRGB(0xf4, 0xf1, 0xea)
}
