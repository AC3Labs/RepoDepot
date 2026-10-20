package agent

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var httpClient = &http.Client{Transport: &http.Transport{
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: 60 * time.Second,
	Proxy:                 http.ProxyFromEnvironment,
}}

// githubHost reports whether a token may be sent to this host. Go drops
// the Authorization header itself when a redirect leaves the host.
func githubHost(u *url.URL) bool {
	h := strings.ToLower(u.Hostname())
	return h == "github.com" || strings.HasSuffix(h, ".github.com") || strings.HasSuffix(h, ".githubusercontent.com")
}

// download saves url to path, logging progress about every 10%.
func download(ctx context.Context, rawURL, token, path string, l *jobLog) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "TheRepoDepot-Agent/"+Version)
	if token != "" && githubHost(req.URL) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if strings.Contains(req.URL.Path, "/releases/assets/") {
		req.Header.Set("Accept", "application/octet-stream")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: %s answered %s", req.URL.Host, resp.Status)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	pw := &progress{total: resp.ContentLength, l: l, start: time.Now()}
	_, err = io.Copy(f, io.TeeReader(resp.Body, pw))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("download failed: %w", err)
	}
	l.Printf("  %s in %.1fs", humanBytes(pw.done), time.Since(pw.start).Seconds())
	return nil
}

// verifySHA256 checks a downloaded file against the checksum GitHub or the
// release published for it, so a damaged or swapped file is never installed.
func verifySHA256(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, strings.TrimSpace(want)) {
		return fmt.Errorf("the download doesn't match its published checksum (expected SHA-256 %s, got %s); nothing was installed", want, got)
	}
	return nil
}

type progress struct {
	total, done int64
	next        int64
	l           *jobLog
	start       time.Time
}

func (p *progress) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.total > 0 && p.done*10/p.total >= p.next+1 && p.done < p.total {
		p.next = p.done * 10 / p.total
		p.l.Printf("  %d%% of %s", p.next*10, humanBytes(p.total))
	}
	return len(b), nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// archiveKind recognises the archive formats the agent can unpack.
func archiveKind(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".zip"):
		return "zip"
	case strings.HasSuffix(n, ".tar.gz"), strings.HasSuffix(n, ".tgz"):
		return "tgz"
	case strings.HasSuffix(n, ".tar.bz2"), strings.HasSuffix(n, ".tbz2"):
		return "tbz"
	case strings.HasSuffix(n, ".tar.xz"), strings.HasSuffix(n, ".txz"):
		return "txz"
	case strings.HasSuffix(n, ".tar"):
		return "tar"
	}
	return ""
}

// extract unpacks archive into dest. stripTop removes the single folder
// most archives wrap their contents in ("auto": only when every entry is
// inside the same folder; "always": GitHub source tarballs).
func extract(archive, kind, dest, stripTop string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	switch kind {
	case "zip":
		return extractZip(archive, dest, stripTop)
	case "txz":
		// No xz in Go's standard library; every Linux and Windows 10+ has tar.
		tmp := dest + ".xz-tmp"
		os.RemoveAll(tmp)
		os.MkdirAll(tmp, 0o755)
		defer os.RemoveAll(tmp)
		if out, err := exec.Command("tar", "-xJf", archive, "-C", tmp).CombinedOutput(); err != nil {
			return fmt.Errorf("tar -xJf: %v: %s", err, out)
		}
		return moveTree(tmp, dest, stripTop)
	default:
		f, err := os.Open(archive)
		if err != nil {
			return err
		}
		defer f.Close()
		var r io.Reader = f
		switch kind {
		case "tgz":
			gz, err := gzip.NewReader(f)
			if err != nil {
				return err
			}
			defer gz.Close()
			r = gz
		case "tbz":
			r = bzip2.NewReader(f)
		}
		return extractTar(r, dest, stripTop, archive, kind)
	}
}

// safeJoin refuses entries that would land outside dest ("../../etc").
func safeJoin(dest, name string) (string, error) {
	name = filepath.FromSlash(strings.TrimLeft(name, "/\\"))
	p := filepath.Join(dest, name)
	if p != dest && !strings.HasPrefix(p, dest+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q points outside the install folder", name)
	}
	return p, nil
}

// cleanEntry drops leading "/" and "./" (not the dot of ".env").
func cleanEntry(name string) string {
	name = filepath.ToSlash(name)
	for {
		switch {
		case strings.HasPrefix(name, "/"):
			name = name[1:]
		case strings.HasPrefix(name, "./"):
			name = name[2:]
		default:
			return name
		}
	}
}

func stripName(name string, strip bool) (string, bool) {
	name = cleanEntry(name)
	if !strip {
		return name, name != ""
	}
	i := strings.Index(name, "/")
	if i < 0 || i == len(name)-1 {
		return "", false // the top folder itself
	}
	return name[i+1:], true
}

func extractTar(r io.Reader, dest, stripTop, archive, kind string) error {
	strip := stripTop == "always"
	if stripTop == "auto" {
		top, err := tarTopFolder(archive, kind)
		if err != nil {
			return err
		}
		strip = top
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name, ok := stripName(h.Name, strip)
		if !ok {
			continue
		}
		p, err := safeJoin(dest, name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFile(p, tr, os.FileMode(h.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Only links that stay inside the folder.
			target := filepath.Join(filepath.Dir(p), filepath.FromSlash(h.Linkname))
			if filepath.IsAbs(h.Linkname) || (target != dest && !strings.HasPrefix(target, dest+string(filepath.Separator))) {
				continue
			}
			os.Remove(p)
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.Symlink(h.Linkname, p)
		}
	}
}

// tarTopFolder reports whether every entry sits under one top folder.
func tarTopFolder(archive, kind string) (bool, error) {
	f, err := os.Open(archive)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var r io.Reader = f
	switch kind {
	case "tgz":
		gz, err := gzip.NewReader(f)
		if err != nil {
			return false, err
		}
		defer gz.Close()
		r = gz
	case "tbz":
		r = bzip2.NewReader(f)
	}
	var names []string
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue // git archive's pax header, not a file
		}
		names = append(names, h.Name)
	}
	return singleTop(names), nil
}

func singleTop(names []string) bool {
	top := ""
	nested := false
	for _, n := range names {
		n = cleanEntry(n)
		if n == "" || n == "." {
			continue
		}
		first, rest, hasSlash := strings.Cut(n, "/")
		if !hasSlash {
			return false // a file at the top level
		}
		if top == "" {
			top = first
		} else if first != top {
			return false
		}
		if rest != "" {
			nested = true
		}
	}
	return top != "" && nested
}

func extractZip(archive, dest, stripTop string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	strip := stripTop == "always"
	if stripTop == "auto" {
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		strip = singleTop(names)
	}
	for _, f := range zr.File {
		name, ok := stripName(f.Name, strip)
		if !ok {
			continue
		}
		p, err := safeJoin(dest, name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if f.Mode()&os.ModeSymlink != 0 {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		mode := f.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		err = writeFile(p, rc, mode)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeFile(p string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if mode&0o200 == 0 {
		mode |= 0o200 // owner must be able to replace it on the next update
	}
	os.Remove(p) // a running program can be replaced, not overwritten (Linux)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// moveTree moves the contents of src into dest, optionally stripping a
// single top folder.
func moveTree(src, dest, stripTop string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if stripTop != "never" && len(entries) == 1 && entries[0].IsDir() {
		return moveTree(filepath.Join(src, entries[0].Name()), dest, "never")
	}
	for _, e := range entries {
		to := filepath.Join(dest, e.Name())
		os.RemoveAll(to)
		if err := os.Rename(filepath.Join(src, e.Name()), to); err != nil {
			return err
		}
	}
	return nil
}
