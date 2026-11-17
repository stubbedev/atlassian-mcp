package gitrepo

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stubbedev/atlassian-mcp/internal/config"
)

const (
	statusUnmodified byte = 0
	statusModified   byte = 'M'
	statusTypeChange byte = 'T'
	statusAdded      byte = 'A'
	statusDeleted    byte = 'D'
	statusUnmerged   byte = 'U'
)

type pathStatus struct{ staged, worktree byte }

func (s pathStatus) dirty() bool {
	return s.staged != statusUnmodified || s.worktree != statusUnmodified
}

var conflictCodes = map[[3]bool]pathStatus{
	{true, false, false}: {statusDeleted, statusDeleted},
	{false, true, false}: {statusAdded, statusUnmerged},
	{true, true, false}:  {statusUnmerged, statusDeleted},
	{false, false, true}: {statusUnmerged, statusAdded},
	{true, false, true}:  {statusDeleted, statusUnmerged},
	{false, true, true}:  {statusAdded, statusAdded},
	{true, true, true}:   {statusUnmerged, statusUnmerged},
}

func trackedStatus(r *git.Repository, root string, idx *index.Index) (map[string]pathStatus, error) {
	head, err := readHeadTree(r, idx)
	if err != nil {
		return nil, err
	}
	staged := make(map[string]*index.Entry, len(idx.Entries))
	conflicts := map[string][3]bool{}
	for _, e := range idx.Entries {
		if e.Stage == 0 {
			staged[e.Name] = e
			continue
		}
		stages := conflicts[e.Name]
		stages[e.Stage-1] = true
		conflicts[e.Name] = stages
	}
	out := map[string]pathStatus{}
	for name, stages := range conflicts {
		code, ok := conflictCodes[stages]
		if !ok {
			return nil, fmt.Errorf("index entry %s has no conflict stages", name)
		}
		out[name] = code
	}
	for name := range head.entries {
		if _, ok := staged[name]; ok {
			continue
		}
		if _, ok := conflicts[name]; ok {
			continue
		}
		out[name] = pathStatus{statusDeleted, statusUnmodified}
	}
	dirs := worktreeDirs{root: root, real: map[string]bool{".": true}}
	for name, ie := range staged {
		var st pathStatus
		if !head.unchangedUnder(name) {
			if he, ok := head.entries[name]; ok {
				st.staged = compareBlobs(he.Hash, he.Mode, ie.Hash, ie.Mode)
			} else {
				st.staged = statusAdded
			}
		}
		wst, err := worktreeStatus(&dirs, ie)
		if err != nil {
			return nil, fmt.Errorf("comparing %s with the index: %w", name, err)
		}
		st.worktree = wst
		if st.dirty() {
			out[name] = st
		}
	}
	return out, nil
}

type headTree struct {
	entries   map[string]object.TreeEntry
	unchanged map[string]bool
}

func (h headTree) unchangedUnder(name string) bool {
	if len(h.unchanged) == 0 {
		return false
	}
	for d := path.Dir(name); ; d = path.Dir(d) {
		if h.unchanged[d] {
			return true
		}
		if d == "." {
			return false
		}
	}
}

func readHeadTree(r *git.Repository, idx *index.Index) (headTree, error) {
	h := headTree{entries: map[string]object.TreeEntry{}, unchanged: map[string]bool{}}
	head, err := r.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return h, nil
	}
	if err != nil {
		return h, fmt.Errorf("reading HEAD: %w", err)
	}
	c, err := r.CommitObject(head.Hash())
	if err != nil {
		return h, fmt.Errorf("reading HEAD commit %s: %w", head.Hash(), err)
	}
	cached, err := cachedTreeHashes(idx.Cache)
	if err != nil {
		return h, err
	}
	if err := h.collect(r, c.TreeHash, ".", cached); err != nil {
		return h, fmt.Errorf("reading tree of %s: %w", head.Hash(), err)
	}
	return h, nil
}

func (h headTree) collect(r *git.Repository, hash plumbing.Hash, dir string, cached map[string]plumbing.Hash) error {
	if c, ok := cached[dir]; ok && c == hash {
		h.unchanged[dir] = true
		return nil
	}
	tree, err := r.TreeObject(hash)
	if err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	for _, e := range tree.Entries {
		name := path.Join(dir, e.Name)
		if e.Mode == filemode.Dir {
			if err := h.collect(r, e.Hash, name, cached); err != nil {
				return err
			}
			continue
		}
		h.entries[name] = e
	}
	return nil
}

func cachedTreeHashes(t *index.Tree) (map[string]plumbing.Hash, error) {
	out := map[string]plumbing.Hash{}
	if t == nil || len(t.Entries) == 0 {
		return out, nil
	}
	next := 0
	var visit func(parent string) error
	visit = func(parent string) error {
		if next >= len(t.Entries) {
			return errors.New("index cache-tree extension lists more subtrees than entries")
		}
		e := t.Entries[next]
		next++
		dir := path.Join(parent, e.Path)
		if e.Entries >= 0 {
			out[dir] = e.Hash
		}
		for range e.Trees {
			if err := visit(dir); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit("."); err != nil {
		return nil, err
	}
	return out, nil
}

func compareBlobs(fromHash plumbing.Hash, fromMode filemode.FileMode, toHash plumbing.Hash, toMode filemode.FileMode) byte {
	switch {
	case fileKind(fromMode) != fileKind(toMode):
		return statusTypeChange
	case fromHash != toHash || fromMode != toMode:
		return statusModified
	default:
		return statusUnmodified
	}
}

func shortCode(c byte) byte {
	if c == statusUnmodified {
		return ' '
	}
	return c
}

func fileKind(m filemode.FileMode) filemode.FileMode {
	if m == filemode.Executable {
		return filemode.Regular
	}
	return m
}

type worktreeDirs struct {
	root string
	real map[string]bool
}

func (d *worktreeDirs) abs(name string) string {
	return filepath.Join(d.root, filepath.FromSlash(name))
}

func (d *worktreeDirs) isRealDir(dir string) (bool, error) {
	if ok, seen := d.real[dir]; seen {
		return ok, nil
	}
	parent, err := d.isRealDir(path.Dir(dir))
	if err != nil || !parent {
		d.real[dir] = false
		return false, err
	}
	info, err := os.Lstat(d.abs(dir))
	switch {
	case errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
		d.real[dir] = false
		return false, nil
	case err != nil:
		return false, err
	}
	d.real[dir] = info.IsDir()
	return info.IsDir(), nil
}

func worktreeStatus(dirs *worktreeDirs, ie *index.Entry) (byte, error) {
	if ie.Mode == filemode.Submodule {
		return statusUnmodified, nil
	}
	parentOK, err := dirs.isRealDir(path.Dir(ie.Name))
	if err != nil {
		return 0, err
	}
	if !parentOK {
		return statusDeleted, nil
	}
	abs := dirs.abs(ie.Name)
	info, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return statusDeleted, nil
	}
	if err != nil {
		return 0, err
	}
	if info.IsDir() {
		return statusDeleted, nil
	}
	mode := worktreeMode(info, ie.Mode)
	if statMatchesIndex(info, mode, ie) {
		return statusUnmodified, nil
	}
	h, err := worktreeBlobHash(abs, info)
	if err != nil {
		return 0, err
	}
	return compareBlobs(ie.Hash, ie.Mode, h, mode), nil
}

func statMatchesIndex(info os.FileInfo, mode filemode.FileMode, ie *index.Entry) bool {
	return mode == ie.Mode && uint32(info.Size()) == ie.Size && info.ModTime().Equal(ie.ModifiedAt) //nolint:gosec // index sizes are mod 2^32
}

func worktreeBlobHash(abs string, info os.FileInfo) (plumbing.Hash, error) {
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(abs)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		return plumbing.ComputeHash(plumbing.BlobObject, []byte(filepath.ToSlash(target))), nil
	}
	f, err := os.Open(abs)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	defer func() { _ = f.Close() }()
	h := plumbing.NewHasher(plumbing.BlobObject, info.Size())
	n, err := io.Copy(h, f)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if n != info.Size() {
		return plumbing.ZeroHash, fmt.Errorf("%s changed size while being hashed", abs)
	}
	return h.Sum(), nil
}

type untrackedWalk struct {
	root        string
	tracked     map[string]bool
	trackedDirs map[string]bool
	out         []string
}

func untrackedPaths(r *git.Repository, root string, idx *index.Index) ([]string, error) {
	base, err := baseIgnorePatterns(r)
	if err != nil {
		return nil, err
	}
	w := &untrackedWalk{root: root, tracked: make(map[string]bool, len(idx.Entries)), trackedDirs: map[string]bool{}}
	for _, e := range idx.Entries {
		w.tracked[e.Name] = true
		for d := path.Dir(e.Name); d != "." && !w.trackedDirs[d]; d = path.Dir(d) {
			w.trackedDirs[d] = true
		}
	}
	if err := w.walkTracked(nil, base); err != nil {
		return nil, err
	}
	return w.out, nil
}

func (w *untrackedWalk) walkTracked(dir []string, ps []gitignore.Pattern) error {
	ps, err := w.withDirIgnores(dir, ps)
	if err != nil {
		return err
	}
	m := gitignore.NewMatcher(ps)
	entries, err := os.ReadDir(w.abs(dir))
	if err != nil {
		return fmt.Errorf("listing %s: %w", displayDir(dir), err)
	}
	for _, e := range entries {
		if e.Name() == gitDirName {
			continue
		}
		parts := append(slices.Clip(dir), e.Name())
		name := strings.Join(parts, "/")
		if w.tracked[name] {
			continue
		}
		if m.Match(parts, e.IsDir()) {
			continue
		}
		if !e.IsDir() {
			w.out = append(w.out, name)
			continue
		}
		if w.trackedDirs[name] {
			if err := w.walkTracked(parts, ps); err != nil {
				return err
			}
			continue
		}
		found, err := w.holdsUntracked(parts, ps)
		if err != nil {
			return err
		}
		if found {
			w.out = append(w.out, name+"/")
		}
	}
	return nil
}

func (w *untrackedWalk) holdsUntracked(dir []string, ps []gitignore.Pattern) (bool, error) {
	ps, err := w.withDirIgnores(dir, ps)
	if err != nil {
		return false, err
	}
	m := gitignore.NewMatcher(ps)
	entries, err := os.ReadDir(w.abs(dir))
	if err != nil {
		return false, fmt.Errorf("listing %s: %w", displayDir(dir), err)
	}
	var subdirs [][]string
	for _, e := range entries {
		if e.Name() == gitDirName {
			return true, nil
		}
		parts := append(slices.Clip(dir), e.Name())
		if m.Match(parts, e.IsDir()) {
			continue
		}
		if !e.IsDir() {
			return true, nil
		}
		subdirs = append(subdirs, parts)
	}
	for _, sub := range subdirs {
		found, err := w.holdsUntracked(sub, ps)
		if err != nil || found {
			return found, err
		}
	}
	return false, nil
}

func (w *untrackedWalk) abs(dir []string) string {
	return filepath.Join(append([]string{w.root}, dir...)...)
}

func (w *untrackedWalk) withDirIgnores(dir []string, ps []gitignore.Pattern) ([]gitignore.Pattern, error) {
	own, err := readIgnoreFile(openOS, filepath.Join(w.abs(dir), gitignoreName), dir)
	if err != nil {
		return nil, err
	}
	if len(own) == 0 {
		return ps, nil
	}
	return append(slices.Clip(ps), own...), nil
}

func displayDir(dir []string) string {
	if len(dir) == 0 {
		return "the worktree root"
	}
	return strings.Join(dir, "/")
}

const (
	gitDirName    = ".git"
	gitignoreName = ".gitignore"
)

func baseIgnorePatterns(r *git.Repository) ([]gitignore.Pattern, error) {
	global, err := globalExcludesPath()
	if err != nil {
		return nil, err
	}
	ps, err := readIgnoreFile(openOS, global, nil)
	if err != nil {
		return nil, err
	}
	dotGit, ok := r.Storer.(interface{ Filesystem() billy.Filesystem })
	if !ok {
		return ps, nil
	}
	local, err := readIgnoreFile(openIn(dotGit.Filesystem()), "info/exclude", nil)
	if err != nil {
		return nil, err
	}
	return append(ps, local...), nil
}

func globalExcludesPath() (string, error) {
	if global, err := gitconfig.LoadConfig(gitconfig.GlobalScope); err == nil {
		if p := config.ExpandHome(global.Raw.Section("core").Option("excludesfile")); p != "" {
			return p, nil
		}
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locating the global git ignore file: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "git", "ignore"), nil
}

func openOS(name string) (io.ReadCloser, error) { return os.Open(name) }

func openIn(fs billy.Filesystem) func(string) (io.ReadCloser, error) {
	return func(name string) (io.ReadCloser, error) { return fs.Open(name) }
}

func readIgnoreFile(open func(string) (io.ReadCloser, error), name string, domain []string) ([]gitignore.Pattern, error) {
	f, err := open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening ignore file %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	var ps []gitignore.Pattern
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ps = append(ps, gitignore.ParsePattern(line, domain))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading ignore file %s: %w", name, err)
	}
	return ps, nil
}

func StatusShort(r *git.Repository) (string, error) {
	wt, err := r.Worktree()
	if err != nil {
		return "", err
	}
	idx, err := r.Storer.Index()
	if err != nil {
		return "", fmt.Errorf("reading the index: %w", err)
	}
	tracked, err := trackedStatus(r, wt.Filesystem.Root(), idx)
	if err != nil {
		return "", err
	}
	untracked, err := untrackedPaths(r, wt.Filesystem.Root(), idx)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(tracked))
	for name := range tracked {
		names = append(names, name)
	}
	sort.Strings(names)
	sort.Strings(untracked)
	lines := make([]string, 0, len(names)+len(untracked))
	for _, name := range names {
		st := tracked[name]
		lines = append(lines, string([]byte{shortCode(st.staged), shortCode(st.worktree), ' '})+name)
	}
	for _, name := range untracked {
		lines = append(lines, "?? "+name)
	}
	return strings.Join(lines, "\n"), nil
}
