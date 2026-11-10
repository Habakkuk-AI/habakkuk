// Command icons writes Habakkuk Desktop's icon files for packaging, from the
// app icon embedded in internal/icon (assets/brand/app-icon-1024.png):
//
//	go run ./cmd/icons -iconset DIR   # DIR/icon_16x16.png ... icon_512x512@2x.png, for iconutil (macOS grid)
//	go run ./cmd/icons -png 256 FILE  # one square PNG (Linux)
//	go run ./cmd/icons -ico FILE      # a Windows .ico, 16 to 256 pixels
//	go run ./cmd/icons -syso FILE -arch amd64 -version v0.3.0
//	                                  # Windows resources for go build: the icon,
//	                                  # packaging/windows/habakkuk-desktop.exe.manifest and version info
package main

import (
	"flag"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"

	"github.com/tc-hib/winres"
	"github.com/tc-hib/winres/version"

	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/icon"
)

func main() {
	iconset := flag.String("iconset", "", "write a macOS .iconset folder")
	size := flag.Int("png", 0, "write one PNG of this size to the file named after the flags")
	ico := flag.String("ico", "", "write a Windows .ico file")
	syso := flag.String("syso", "", "write a Windows resource object (.syso) for go build")
	arch := flag.String("arch", "amd64", "with -syso: amd64, arm64 or 386")
	ver := flag.String("version", "dev", "with -syso: the version for the version info")
	manifest := flag.String("manifest", "packaging/windows/habakkuk-desktop.exe.manifest", "with -syso: the application manifest")
	flag.Parse()
	var err error
	switch {
	case *iconset != "":
		err = writeIconset(*iconset)
	case *size > 0 && flag.Arg(0) != "":
		err = os.WriteFile(flag.Arg(0), icon.App(*size), 0o644)
	case *ico != "":
		err = writeICO(*ico)
	case *syso != "":
		err = writeSyso(*syso, *arch, *ver, *manifest)
	default:
		err = fmt.Errorf("give -iconset DIR, -png SIZE FILE, -ico FILE or -syso FILE")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "icons:", err)
		os.Exit(1)
	}
}

// writeIconset writes every size iconutil wants, on the macOS icon grid.
func writeIconset(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, n := range []int{16, 32, 128, 256, 512} {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("icon_%dx%d.png", n, n)), icon.MacApp(n), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("icon_%dx%d@2x.png", n, n)), icon.MacApp(2*n), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// windowsIcon holds the sizes Windows picks from: 16 to 256 pixels.
func windowsIcon() (*winres.Icon, error) {
	var imgs []image.Image
	for _, n := range []int{16, 20, 24, 32, 40, 48, 64, 128, 256} {
		imgs = append(imgs, icon.Image(n))
	}
	return winres.NewIconFromImages(imgs)
}

func writeICO(file string) error {
	ic, err := windowsIcon()
	if err != nil {
		return err
	}
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	if err := ic.SaveICO(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func writeSyso(file, arch, ver, manifestFile string) error {
	archs := map[string]winres.Arch{"amd64": winres.ArchAMD64, "arm64": winres.ArchARM64, "386": winres.ArchI386}
	a, ok := archs[arch]
	if !ok {
		return fmt.Errorf("arch %q not supported", arch)
	}
	var rs winres.ResourceSet
	ic, err := windowsIcon()
	if err != nil {
		return err
	}
	// The ID the Wails runtime loads the window and tray icon from.
	if err := rs.SetIcon(winres.RT_ICON, ic); err != nil {
		return err
	}
	xml, err := os.ReadFile(manifestFile)
	if err != nil {
		return err
	}
	m, err := winres.AppManifestFromXML(xml)
	if err != nil {
		return err
	}
	rs.SetManifest(m)

	v := strings.TrimPrefix(ver, "v")
	if v == "" || v[0] < '0' || v[0] > '9' {
		v = "0.0.0" // dev builds
	}
	var vi version.Info
	vi.SetFileVersion(v)
	vi.SetProductVersion(v)
	for k, val := range map[string]string{
		version.ProductName:      "Habakkuk",
		version.FileDescription:  "Habakkuk",
		version.InternalName:     "habakkuk-desktop",
		version.OriginalFilename: "habakkuk-desktop.exe",
		version.ProductVersion:   ver,
		version.FileVersion:      ver,
		version.LegalCopyright:   "Copyright 2026 Ayobami Joseph and the Habakkuk contributors. Apache License 2.0.",
	} {
		if err := vi.Set(version.LangDefault, k, val); err != nil {
			return err
		}
	}
	rs.SetVersionInfo(vi)

	f, err := os.Create(file)
	if err != nil {
		return err
	}
	if err := rs.WriteObject(f, a); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
