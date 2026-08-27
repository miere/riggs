package updates

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// installerOver builds an Installer whose "running binary" is a file in a temp
// directory, so a swap can be asserted on without touching the real one.
func installerOver(t *testing.T, s *stubGet, current string) (*Installer, string) {
	t.Helper()
	// Resolved, because on macOS t.TempDir() hands back a path under /var,
	// which is itself a symlink to /private/var — and the installer resolves
	// what it is replacing. Comparing against the unresolved path would fail
	// for a reason that has nothing to do with the swap.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "riggs")
	if err := os.WriteFile(bin, []byte("the old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	i := NewInstaller(checkerFor(t, current, s))
	i.selfPath = func() (string, error) { return bin, nil }
	i.goos, i.goarch = "darwin", "arm64"
	// The real verify runs the staged file; these fixtures are not executables.
	i.verify = func(context.Context, string) error { return nil }
	return i, bin
}

// assetStub answers both the tag lookup and the asset download.
func assetStub(tag, payload string) *stubGet {
	tagURL := "https://api.github.com/repos/miere/riggs/releases/tags/" + tag
	return &stubGet{bodies: map[string]string{
		latestURL: releaseJSON(tag, "notes"),
		tagURL: `{"tag_name":"` + tag + `","assets":[
			{"name":"riggs-` + tag + `-darwin-arm64","browser_download_url":"https://example.test/asset"}]}`,
		"https://example.test/asset": payload,
	}}
}

func TestInstallSwapsTheBinaryAndKeepsABackup(t *testing.T) {
	i, bin := installerOver(t, assetStub("v0.2.0", "the new binary"), "v0.1.0")

	result, err := i.Install(context.Background(), "v0.2.0")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if result.Tag != "v0.2.0" || result.Path != bin {
		t.Fatalf("result = %+v", result)
	}

	installed, _ := os.ReadFile(bin)
	if string(installed) != "the new binary" {
		t.Fatalf("installed binary = %q", installed)
	}
	kept, err := os.ReadFile(result.BackupPath)
	if err != nil || string(kept) != "the old binary" {
		t.Fatalf("backup = %q, %v", kept, err)
	}
	// The new binary must be executable, or launchd's next spawn fails with a
	// permission error nobody will connect to this.
	info, _ := os.Stat(bin)
	if info.Mode()&0o111 == 0 {
		t.Fatalf("installed binary is not executable: %v", info.Mode())
	}
}

// An empty tag installs whatever the checker reports as latest — which is what
// the Home tab's button asks for, since the tag is resolved at click time.
func TestInstallWithNoTagTakesTheLatest(t *testing.T) {
	i, bin := installerOver(t, assetStub("v0.2.0", "the new binary"), "dev")

	result, err := i.Install(context.Background(), "")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if result.Tag != "v0.2.0" {
		t.Fatalf("result = %+v, want the latest release", result)
	}
	if installed, _ := os.ReadFile(bin); string(installed) != "the new binary" {
		t.Fatalf("installed = %q", installed)
	}
}

// The whole point of staging: a download that is not a working Riggs — an HTML
// error page, a truncated file, the wrong architecture — must leave the running
// binary exactly where it was.
func TestAFailedVerifyLeavesTheOriginalInPlace(t *testing.T) {
	i, bin := installerOver(t, assetStub("v0.2.0", "<html>404</html>"), "v0.1.0")
	i.verify = func(context.Context, string) error { return errors.New("exec format error") }

	if _, err := i.Install(context.Background(), "v0.2.0"); err == nil {
		t.Fatal("Install accepted a binary that failed verification")
	}
	if kept, _ := os.ReadFile(bin); string(kept) != "the old binary" {
		t.Fatalf("the running binary was replaced anyway: %q", kept)
	}
	// And nothing is left lying around beside it.
	entries, _ := os.ReadDir(filepath.Dir(bin))
	if len(entries) != 1 {
		t.Fatalf("staging left %d files behind, want just the binary", len(entries))
	}
}

func TestAFailedDownloadInstallsNothing(t *testing.T) {
	s := assetStub("v0.2.0", "the new binary")
	delete(s.bodies, "https://example.test/asset")
	i, bin := installerOver(t, s, "v0.1.0")

	if _, err := i.Install(context.Background(), "v0.2.0"); err == nil {
		t.Fatal("Install reported success with no asset downloaded")
	}
	if kept, _ := os.ReadFile(bin); string(kept) != "the old binary" {
		t.Fatalf("the running binary was replaced: %q", kept)
	}
}

// A symlinked binary must have its target replaced. Renaming over the link
// itself turns it into a regular file, and whatever else pointed at the target
// silently keeps the old version.
func TestASymlinkedBinaryHasItsTargetReplaced(t *testing.T) {
	i, bin := installerOver(t, assetStub("v0.2.0", "the new binary"), "v0.1.0")
	link := filepath.Join(t.TempDir(), "riggs")
	if err := os.Symlink(bin, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	i.selfPath = func() (string, error) { return link, nil }

	result, err := i.Install(context.Background(), "v0.2.0")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if result.Path != bin {
		t.Fatalf("replaced %q, want the symlink's target %q", result.Path, bin)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was clobbered: %v, %v", info.Mode(), err)
	}
}

// verifyByRunning is the live probe. It has to reject a file that is not an
// executable at all, which is what a truncated or HTML "download" looks like.
func TestVerifyByRunningRejectsSomethingThatIsNotABinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-riggs")
	if err := os.WriteFile(path, []byte("<html>404</html>"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyByRunning(context.Background(), path); err == nil {
		t.Fatal("verifyByRunning accepted an HTML page")
	}
}
