package updates

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// HTTPGetter is the live HTTPGet: a context-aware GET that follows GitHub's
// redirect to the asset's storage host.
//
// The Accept header matters. Without it the release-asset endpoint returns the
// asset's JSON metadata rather than its bytes, and the "binary" that lands on
// disk is a JSON document that fails verification with a baffling message.
func HTTPGetter() HTTPGet {
	client := &http.Client{Timeout: 5 * time.Minute}
	return func(ctx context.Context, url string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		if strings.Contains(url, "/releases/assets/") || strings.Contains(url, "/releases/download/") {
			req.Header.Set("Accept", "application/octet-stream")
		} else {
			req.Header.Set("Accept", "application/vnd.github+json")
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}
}

// Installer replaces the running binary with a release asset.
//
// The swap is staged: the asset is written beside the target, made executable,
// and run once to prove it is a working Riggs before anything is moved. A
// download that is truncated, HTML, or built for the wrong architecture fails
// there, with the original binary still in place and the daemon still running
// it. Only then is the old binary backed up and the new one renamed over it —
// a rename within one directory, so there is no window in which the path does
// not resolve.
type Installer struct {
	checker *Checker
	// selfPath returns the binary to replace; nil selects os.Executable.
	selfPath func() (string, error)
	// verify proves a staged file is a working Riggs; nil selects
	// verifyByRunning.
	verify func(ctx context.Context, path string) error
	// goos and goarch select the asset. Empty selects the build's own.
	goos, goarch string
}

// NewInstaller builds an Installer over a Checker.
func NewInstaller(c *Checker) *Installer {
	return &Installer{checker: c, goos: runtime.GOOS, goarch: runtime.GOARCH}
}

// Result is what an install did.
type Result struct {
	// Tag is the release now on disk.
	Tag string
	// Path is the binary that was replaced.
	Path string
	// BackupPath is where the previous binary was kept, empty when there was
	// none to keep.
	BackupPath string
}

// Install downloads the release asset for tag and swaps it in.
//
// An empty tag installs the latest release. There is deliberately NO "you are
// already current" short-circuit and no dev-build refusal: the button is only
// rendered when the checker says there is something to install, and a Riggs
// built from a working tree must be able to take a stable release (see the
// package comment).
func (i *Installer) Install(ctx context.Context, tag string) (Result, error) {
	if i.checker == nil {
		return Result{}, fmt.Errorf("updates: no checker configured")
	}
	if strings.TrimSpace(tag) == "" {
		rel, err := i.checker.Check(ctx)
		if err != nil && rel.Tag == "" {
			return Result{}, fmt.Errorf("resolving the latest release: %w", err)
		}
		tag = rel.Tag
	}
	if strings.TrimSpace(tag) == "" {
		return Result{}, fmt.Errorf("updates: no release to install")
	}

	goos, goarch := i.platform()
	assetURL, err := i.checker.AssetURL(ctx, tag, goos, goarch)
	if err != nil {
		return Result{}, err
	}
	asset, err := i.checker.httpGet(ctx, assetURL)
	if err != nil {
		return Result{}, fmt.Errorf("download %s: %w", tag, err)
	}
	return i.swap(ctx, tag, asset)
}

// platform returns the (goos, goarch) pair the asset is selected by.
func (i *Installer) platform() (string, string) {
	goos, goarch := i.goos, i.goarch
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return goos, goarch
}

// swap stages, verifies and installs the downloaded asset.
func (i *Installer) swap(ctx context.Context, tag string, asset []byte) (Result, error) {
	self := i.selfPath
	if self == nil {
		self = os.Executable
	}
	binPath, err := self()
	if err != nil {
		return Result{}, fmt.Errorf("locating the running binary: %w", err)
	}
	// Resolved, because a symlinked binary should have its *target* replaced —
	// otherwise the swap silently turns the symlink into a regular file and the
	// next `riggs launchd install` points at something else.
	if resolved, err := filepath.EvalSymlinks(binPath); err == nil {
		binPath = resolved
	}

	tmp, err := os.CreateTemp(filepath.Dir(binPath), ".riggs-update-*")
	if err != nil {
		return Result{}, fmt.Errorf("staging the update beside %s: %w", binPath, err)
	}
	tmpPath := tmp.Name()
	discard := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(asset); err != nil {
		tmp.Close()
		discard()
		return Result{}, fmt.Errorf("writing the staged update: %w", err)
	}
	if err := tmp.Close(); err != nil {
		discard()
		return Result{}, fmt.Errorf("closing the staged update: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		discard()
		return Result{}, fmt.Errorf("making the staged update executable: %w", err)
	}

	verify := i.verify
	if verify == nil {
		verify = verifyByRunning
	}
	if err := verify(ctx, tmpPath); err != nil {
		discard()
		return Result{}, fmt.Errorf("the downloaded %s is not a working riggs, so nothing was replaced: %w", tag, err)
	}

	backup, err := backupExisting(binPath)
	if err != nil {
		discard()
		return Result{}, err
	}
	if err := os.Rename(tmpPath, binPath); err != nil {
		discard()
		return Result{}, fmt.Errorf("installing %s at %s: %w", tag, binPath, err)
	}
	return Result{Tag: tag, Path: binPath, BackupPath: backup}, nil
}

// verifyByRunning proves the staged file is a Riggs by asking it its version.
//
// `version` is the right probe because it is the one subcommand that touches
// nothing: no config file, no Slack, no ledger. A binary for the wrong
// architecture, a truncated download or an HTML error page all fail here.
func verifyByRunning(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s version: %w: %s", filepath.Base(path), err, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("%s version printed nothing", filepath.Base(path))
	}
	return nil
}

// backupExisting copies the current binary aside before it is replaced.
//
// A copy rather than a rename: renaming the running binary out of the way is
// legal on Unix and leaves the daemon running from a path that no longer holds
// what it says it does — which is precisely the failure mode that once left a
// daemon running from a deleted temp file for nineteen hours.
//
// A missing binary is not an error. There is simply nothing to keep.
func backupExisting(path string) (string, error) {
	src, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the current binary at %s: %w", path, err)
	}
	defer src.Close()

	backup := path + ".backup"
	dst, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return "", fmt.Errorf("creating the backup at %s: %w", backup, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return "", fmt.Errorf("writing the backup at %s: %w", backup, err)
	}
	if err := dst.Close(); err != nil {
		return "", fmt.Errorf("closing the backup at %s: %w", backup, err)
	}
	return backup, nil
}
