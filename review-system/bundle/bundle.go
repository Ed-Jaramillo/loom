// Package bundle exports a frozen review input (delivery row W1.1.2). It pins
// the head, base tip and diff base to exact SHAs, lists changed files and
// hashes each file's content at the pinned head. It fails when the source
// changes during the export or when a file listing is incomplete.
package bundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	ErrRevisionChanged = errors.New("bundle: source revision changed during export")
	ErrIncomplete      = errors.New("bundle: source file listing is incomplete")
	ErrInvalidRef      = errors.New("bundle: invalid ref")
	ErrInvalidPath     = errors.New("bundle: invalid path")
)

// ManifestVersion is the schema version of Manifest.
const ManifestVersion = 1

// Snapshot holds the pinned revisions of one review input.
type Snapshot struct {
	HeadSHA     string `json:"head_sha"`
	BaseTipSHA  string `json:"base_tip_sha"`
	DiffBaseSHA string `json:"diff_base_sha"`
}

// ChangedFile is one entry in the changed-file list. Status is A, M, D or R.
// SHA256 is empty for deleted files.
type ChangedFile struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	Status  string `json:"status"`
	SHA256  string `json:"sha256,omitempty"`
}

// FilePage is one listing result. Complete must be true before the list can be
// used as evidence.
type FilePage struct {
	Files    []ChangedFile
	Complete bool
}

// Source provides the reads that an export needs. Implementations must return
// content for the pinned Snapshot, not for whatever the refs point to now.
type Source interface {
	Snapshot(ctx context.Context) (Snapshot, error)
	ChangedFiles(ctx context.Context, s Snapshot) (FilePage, error)
	ReadFile(ctx context.Context, s Snapshot, path string) ([]byte, error)
}

// Manifest is the frozen bundle. BundleSHA256 covers every other field.
type Manifest struct {
	Version      int           `json:"version"`
	Snapshot     Snapshot      `json:"snapshot"`
	Files        []ChangedFile `json:"files"`
	BundleSHA256 string        `json:"bundle_sha256"`
}

// Export pins the source, lists changed files and hashes their content. It
// reads the snapshot again at the end and fails if any revision changed.
func Export(ctx context.Context, src Source) (Manifest, error) {
	s1, err := src.Snapshot(ctx)
	if err != nil {
		return Manifest{}, fmt.Errorf("bundle: snapshot: %w", err)
	}
	page, err := src.ChangedFiles(ctx, s1)
	if err != nil {
		return Manifest{}, fmt.Errorf("bundle: list changed files: %w", err)
	}
	if !page.Complete {
		return Manifest{}, ErrIncomplete
	}

	files := make([]ChangedFile, len(page.Files))
	copy(files, page.Files)
	for i := range files {
		if files[i].Status == "D" {
			continue
		}
		content, err := src.ReadFile(ctx, s1, files[i].Path)
		if err != nil {
			return Manifest{}, fmt.Errorf("bundle: read %s: %w", files[i].Path, err)
		}
		sum := sha256.Sum256(content)
		files[i].SHA256 = hex.EncodeToString(sum[:])
	}

	s2, err := src.Snapshot(ctx)
	if err != nil {
		return Manifest{}, fmt.Errorf("bundle: second snapshot: %w", err)
	}
	if s2 != s1 {
		return Manifest{}, ErrRevisionChanged
	}

	sort.Slice(files, func(i, j int) bool {
		if files[i].Path != files[j].Path {
			return files[i].Path < files[j].Path
		}
		return files[i].OldPath < files[j].OldPath
	})

	m := Manifest{Version: ManifestVersion, Snapshot: s1, Files: files}
	digest, err := digestOf(m)
	if err != nil {
		return Manifest{}, err
	}
	m.BundleSHA256 = digest
	return m, nil
}

// Verify recomputes the digest of a manifest and compares it with the stored value.
func Verify(m Manifest) error {
	want, err := digestOf(m)
	if err != nil {
		return err
	}
	if want != m.BundleSHA256 {
		return fmt.Errorf("bundle: digest mismatch: stored %s, computed %s", m.BundleSHA256, want)
	}
	return nil
}

func digestOf(m Manifest) (string, error) {
	m.BundleSHA256 = ""
	raw, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("bundle: encode manifest: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// WriteManifest writes the manifest as indented JSON, replacing the file atomically.
func WriteManifest(path string, m Manifest) error {
	if err := Verify(m); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("bundle: encode manifest: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".manifest-*.json")
	if err != nil {
		return fmt.Errorf("bundle: create temp manifest: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("bundle: write temp manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("bundle: close temp manifest: %w", err)
	}
	return os.Rename(tmp.Name(), path)
}

var refPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,255}$`)

// GitSource reads a local clone. Refs are resolved with rev-parse, so no fetch
// runs and the subject checkout is not changed. Git runs with optional locks disabled.
type GitSource struct {
	RepoDir string
	HeadRef string
	BaseRef string
}

// Snapshot resolves both refs to commits and computes the merge base.
func (g GitSource) Snapshot(ctx context.Context) (Snapshot, error) {
	if !validRef(g.HeadRef) || !validRef(g.BaseRef) {
		return Snapshot{}, ErrInvalidRef
	}
	head, err := g.revParse(ctx, g.HeadRef)
	if err != nil {
		return Snapshot{}, err
	}
	base, err := g.revParse(ctx, g.BaseRef)
	if err != nil {
		return Snapshot{}, err
	}
	mb, err := g.run(ctx, "merge-base", head, base)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		HeadSHA:     head,
		BaseTipSHA:  base,
		DiffBaseSHA: strings.TrimSpace(string(mb)),
	}, nil
}

// ChangedFiles lists files changed from the diff base to the head, with rename detection.
func (g GitSource) ChangedFiles(ctx context.Context, s Snapshot) (FilePage, error) {
	out, err := g.run(ctx, "diff", "--no-color", "--name-status", "-z", "-M", s.DiffBaseSHA, s.HeadSHA)
	if err != nil {
		return FilePage{}, err
	}
	fields := strings.Split(string(out), "\x00")
	var files []ChangedFile
	for i := 0; i < len(fields); {
		status := fields[i]
		if status == "" {
			i++
			continue
		}
		switch status[0] {
		case 'R', 'C':
			if i+2 >= len(fields) {
				return FilePage{}, errors.New("bundle: malformed rename record in diff output")
			}
			files = append(files, ChangedFile{Path: fields[i+2], OldPath: fields[i+1], Status: "R"})
			i += 3
		default:
			if i+1 >= len(fields) {
				return FilePage{}, errors.New("bundle: malformed record in diff output")
			}
			files = append(files, ChangedFile{Path: fields[i+1], Status: string(status[0])})
			i += 2
		}
	}
	// Git lists every changed path in one call. A missing page is therefore not possible here.
	return FilePage{Files: files, Complete: true}, nil
}

// ReadFile returns the blob at the pinned head for path.
func (g GitSource) ReadFile(ctx context.Context, s Snapshot, path string) ([]byte, error) {
	if !validPath(path) {
		return nil, ErrInvalidPath
	}
	return g.run(ctx, "cat-file", "blob", s.HeadSHA+":"+path)
}

func (g GitSource) revParse(ctx context.Context, ref string) (string, error) {
	out, err := g.run(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("bundle: resolve %s: %w", ref, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (g GitSource) run(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"-C", g.RepoDir, "-c", "core.quotepath=off"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func validRef(ref string) bool {
	return refPattern.MatchString(ref) && !strings.HasPrefix(ref, "-") && !strings.Contains(ref, "..")
}

func validPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "-") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
