package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// Release channels: stable follows GitHub's latest release (never a
// pre-release); beta takes the newest release of any kind, so it gets
// v1.4.0-beta.1 before v1.4.0 ships.
const (
	channelStable = "stable"
	channelBeta   = "beta"
)

type release struct {
	Tag        string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []asset `json:"assets"`
}

type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

func (r *release) asset(name string) string {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

const (
	maxArchive = 256 << 20
	maxSums    = 64 << 10
)

// errNoRelease means the channel has nothing published. GitHub answers 404
// for /releases/latest until a repository has its first stable release.
var errNoRelease = errors.New("no release has been published yet")

// statusError is a response other than 200.
type statusError struct {
	Code int
	URL  string
}

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d from %s", e.Code, e.URL) }

// latest finds the release the channel points at.
func (u *updater) latest(ctx context.Context, channel string) (*release, error) {
	switch channel {
	case channelStable, "":
		var rel release
		if err := u.getJSON(ctx, u.API+"/releases/latest", &rel); err != nil {
			var se *statusError
			if errors.As(err, &se) && se.Code == http.StatusNotFound {
				return nil, errNoRelease
			}
			return nil, fmt.Errorf("find the latest release: %w", err)
		}
		if rel.Tag == "" {
			return nil, errors.New("the latest release has no tag")
		}
		return &rel, nil
	case channelBeta:
		var rels []release
		if err := u.getJSON(ctx, u.API+"/releases?per_page=30", &rels); err != nil {
			return nil, fmt.Errorf("list releases: %w", err)
		}
		var best *release
		for i := range rels {
			r := &rels[i]
			if r.Draft || r.Tag == "" {
				continue
			}
			if best == nil || compareVersions(r.Tag, best.Tag) > 0 {
				best = r
			}
		}
		if best == nil {
			return nil, errNoRelease
		}
		return best, nil
	}
	return nil, fmt.Errorf("unknown channel %q (stable or beta)", channel)
}

// assetName is the archive for a platform: a .zip on Windows, else .tar.gz.
func assetName(goos, arch string) string {
	if goos == "windows" {
		return fmt.Sprintf("habakkuk_%s_%s.zip", goos, arch)
	}
	return fmt.Sprintf("habakkuk_%s_%s.tar.gz", goos, arch)
}

// download fetches this platform's archive from rel, checks it against
// SHA256SUMS (and SHA256SUMS.sig when the release or this build has a
// signing key), and returns the binary inside.
func (u *updater) download(ctx context.Context, rel *release) ([]byte, error) {
	name := assetName(u.OS, u.Arch)
	archiveURL, sumsURL, sigURL := rel.asset(name), rel.asset("SHA256SUMS"), rel.asset("SHA256SUMS.sig")
	if archiveURL == "" {
		return nil, fmt.Errorf("release %s has no build for %s/%s", rel.Tag, u.OS, u.Arch)
	}
	if sumsURL == "" {
		return nil, fmt.Errorf("release %s has no SHA256SUMS; refusing to install an unverified binary", rel.Tag)
	}
	sums, err := u.get(ctx, sumsURL, maxSums)
	if err != nil {
		return nil, fmt.Errorf("download SHA256SUMS: %w", err)
	}
	switch {
	case sigURL != "" && len(u.PublicKey) > 0:
		sig, err := u.get(ctx, sigURL, 4<<10)
		if err != nil {
			return nil, fmt.Errorf("download SHA256SUMS.sig: %w", err)
		}
		if err := verifySignature(u.PublicKey, sums, sig); err != nil {
			return nil, fmt.Errorf("release %s: %w; not installing it", rel.Tag, err)
		}
		fmt.Fprintln(u.Out, "signature: ok")
	case len(u.PublicKey) > 0:
		return nil, fmt.Errorf("release %s is not signed (no SHA256SUMS.sig) but this build expects signed releases; not installing it", rel.Tag)
	case sigURL != "":
		fmt.Fprintln(u.Out, "signature: present, but this build has no release key to check it with; relying on the checksum")
	}
	want, err := checksumFor(sums, name)
	if err != nil {
		return nil, err
	}
	archive, err := u.get(ctx, archiveURL, maxArchive)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("%s does not match SHA256SUMS; not installing it", name)
	}
	fmt.Fprintf(u.Out, "checksum: ok (%s)\n", name)
	if strings.HasSuffix(name, ".zip") {
		return extractZipBinary(archive, "habakkuk.exe")
	}
	return extractBinary(archive)
}

// verifySignature checks an Ed25519 signature (base64, as the release
// workflow writes it) over SHA256SUMS.
func verifySignature(key ed25519.PublicKey, sums, sig []byte) error {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return errors.New("SHA256SUMS.sig is not a valid signature")
	}
	if !ed25519.Verify(key, sums, raw) {
		return errors.New("SHA256SUMS.sig does not match the release signing key")
	}
	return nil
}

// parsePublicKey decodes a base64 Ed25519 public key ("" gives nil).
func parsePublicKey(s string) (ed25519.PublicKey, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("the release public key is not a base64 Ed25519 key")
	}
	return ed25519.PublicKey(raw), nil
}

func (u *updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "habakkuk/"+u.Current)
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{Code: resp.StatusCode, URL: url}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d MB", url, limit>>20)
	}
	return data, nil
}

func (u *updater) getJSON(ctx context.Context, url string, v any) error {
	data, err := u.get(ctx, url, 1<<20)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// checksumFor finds a file's hash in sha256sum output.
func checksumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS lists no %s", name)
}

// extractBinary returns the "habakkuk" file from a release .tar.gz.
func extractBinary(archive []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the archive holds no habakkuk binary")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == "habakkuk" {
			return readBinary(tr)
		}
	}
}

// extractZipBinary returns the named file from a release .zip.
func extractZipBinary(archive []byte, name string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if path.Base(f.Name) != name || f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return readBinary(rc)
	}
	return nil, fmt.Errorf("the archive holds no %s", name)
}

func readBinary(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxArchive))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("the habakkuk binary in the archive is empty")
	}
	return data, nil
}

// compareVersions orders versions like v1.2.3 and 1.2.3-rc1 by their
// numbers; a pre-release sorts before its release, and a version that is
// not numbered (a dev build) before any release.
func compareVersions(a, b string) int {
	pa, preA, okA := parseVersion(a)
	pb, preB, okB := parseVersion(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	for i := range 3 {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case preA == preB:
		return 0
	case preA == "":
		return 1
	case preB == "":
		return -1
	case preA < preB:
		return -1
	}
	return 1
}

func parseVersion(s string) ([3]int, string, bool) {
	var out [3]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	core, pre, _ := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, "", false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, "", false
		}
		out[i] = n
	}
	return out, pre, true
}
