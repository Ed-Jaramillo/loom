package bundle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// gitIn runs git in dir with a fixed identity and no signing.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir,
		"-c", "user.name=test", "-c", "user.email=test@example.com",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func lines(n int, prefix string) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%s line %d\n", prefix, i)
	}
	return b.String()
}

// newScenario builds a repo whose feature branch changes, adds, renames and
// deletes files. Main advances after the branch point, so the merge base
// differs from the base tip.
func newScenario(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "a.txt", lines(40, "a"))
	writeFile(t, dir, "c.txt", lines(40, "c"))
	writeFile(t, dir, "e.txt", "gone\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "base")

	gitIn(t, dir, "checkout", "-q", "-b", "feature")
	writeFile(t, dir, "a.txt", lines(40, "a")+"changed\n")
	writeFile(t, dir, "b.go", "package b\n")
	gitIn(t, dir, "mv", "c.txt", "d.txt")
	gitIn(t, dir, "rm", "-q", "e.txt")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "feature")

	gitIn(t, dir, "checkout", "-q", "main")
	writeFile(t, dir, "main-only.txt", "advance\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "main advances")
	gitIn(t, dir, "checkout", "-q", "feature")
	return dir
}

func TestExportPinsRevisionsAndListsFiles(t *testing.T) {
	dir := newScenario(t)
	src := GitSource{RepoDir: dir, HeadRef: "feature", BaseRef: "main"}

	m, err := Export(context.Background(), src)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if err := Verify(m); err != nil {
		t.Fatalf("verify: %v", err)
	}
	head := gitIn(t, dir, "rev-parse", "feature")
	base := gitIn(t, dir, "rev-parse", "main")
	mergeBase := gitIn(t, dir, "merge-base", "feature", "main")
	if m.Snapshot.HeadSHA != head || m.Snapshot.BaseTipSHA != base || m.Snapshot.DiffBaseSHA != mergeBase {
		t.Fatalf("snapshot = %+v; want head %s base %s merge-base %s", m.Snapshot, head, base, mergeBase)
	}
	if m.Snapshot.DiffBaseSHA == m.Snapshot.BaseTipSHA {
		t.Fatal("scenario should give a merge base different from the base tip")
	}

	byPath := map[string]ChangedFile{}
	for _, f := range m.Files {
		byPath[f.Path] = f
	}
	if got := byPath["a.txt"]; got.Status != "M" || got.SHA256 == "" {
		t.Errorf("a.txt = %+v, want modified with hash", got)
	}
	if got := byPath["b.go"]; got.Status != "A" || got.SHA256 == "" {
		t.Errorf("b.go = %+v, want added with hash", got)
	}
	if got := byPath["d.txt"]; got.Status != "R" || got.OldPath != "c.txt" || got.SHA256 == "" {
		t.Errorf("d.txt = %+v, want rename from c.txt with hash", got)
	}
	if got, ok := byPath["e.txt"]; !ok || got.Status != "D" || got.SHA256 != "" {
		t.Errorf("e.txt = %+v, want deletion without hash", got)
	}
	if _, ok := byPath["main-only.txt"]; ok {
		t.Error("base-only change leaked into the changed-file list")
	}
}

func TestRepeatExportMatches(t *testing.T) {
	dir := newScenario(t)
	src := GitSource{RepoDir: dir, HeadRef: "feature", BaseRef: "main"}
	first, err := Export(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Export(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if first.BundleSHA256 != second.BundleSHA256 || !reflect.DeepEqual(first, second) {
		t.Fatalf("repeat export differs:\n%+v\n%+v", first, second)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	dir := newScenario(t)
	m, err := Export(context.Background(), GitSource{RepoDir: dir, HeadRef: "feature", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	m.Files[0].SHA256 = strings.Repeat("0", 64)
	if err := Verify(m); err == nil {
		t.Fatal("verify accepted an edited manifest")
	}
}

// movingSource moves the head ref after the first read, which simulates a PR
// head push during collection.
type movingSource struct {
	GitSource
	t     *testing.T
	moved bool
}

func (m *movingSource) ReadFile(ctx context.Context, s Snapshot, path string) ([]byte, error) {
	if !m.moved {
		m.moved = true
		writeFile(m.t, m.RepoDir, "pushed.txt", "late push\n")
		gitIn(m.t, m.RepoDir, "add", "pushed.txt")
		gitIn(m.t, m.RepoDir, "commit", "-q", "-m", "late push")
	}
	return m.GitSource.ReadFile(ctx, s, path)
}

func TestExportFailsWhenHeadMovesDuringExport(t *testing.T) {
	dir := newScenario(t)
	src := &movingSource{GitSource: GitSource{RepoDir: dir, HeadRef: "feature", BaseRef: "main"}, t: t}
	_, err := Export(context.Background(), src)
	if !errors.Is(err, ErrRevisionChanged) {
		t.Fatalf("error = %v, want ErrRevisionChanged", err)
	}
}

type partialSource struct{ GitSource }

func (p partialSource) ChangedFiles(ctx context.Context, s Snapshot) (FilePage, error) {
	page, err := p.GitSource.ChangedFiles(ctx, s)
	page.Complete = false
	return page, err
}

func TestExportFailsOnIncompleteListing(t *testing.T) {
	dir := newScenario(t)
	src := partialSource{GitSource{RepoDir: dir, HeadRef: "feature", BaseRef: "main"}}
	if _, err := Export(context.Background(), src); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("error = %v, want ErrIncomplete", err)
	}
}

func TestGitSourceRejectsUnsafeInputs(t *testing.T) {
	dir := newScenario(t)
	ctx := context.Background()
	if _, err := (GitSource{RepoDir: dir, HeadRef: "--upload-pack=x", BaseRef: "main"}).Snapshot(ctx); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("flag-like ref: error = %v, want ErrInvalidRef", err)
	}
	src := GitSource{RepoDir: dir, HeadRef: "feature", BaseRef: "main"}
	snap, err := src.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../outside", "/etc/passwd", "a//b", "-x", "a/../b", ""} {
		if _, err := src.ReadFile(ctx, snap, p); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("path %q: error = %v, want ErrInvalidPath", p, err)
		}
	}
}

func TestGitSourceMissingRefFailsExplicitly(t *testing.T) {
	dir := newScenario(t)
	_, err := GitSource{RepoDir: dir, HeadRef: "no-such-branch", BaseRef: "main"}.Snapshot(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no-such-branch") {
		t.Fatalf("error = %v, want one naming the missing ref", err)
	}
}

func TestWriteManifestRoundTrip(t *testing.T) {
	dir := newScenario(t)
	m, err := Export(context.Background(), GitSource{RepoDir: dir, HeadRef: "feature", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := WriteManifest(path, m); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
