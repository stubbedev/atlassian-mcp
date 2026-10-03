package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// safeCheckout switches HEAD to branch the way `git checkout <branch>` does.
// Only the files that differ between the current commit and the target are
// rewritten, so unrelated local changes — staged or not — carry over. When the
// switch would overwrite a file with local changes, or an untracked or ignored
// file, it refuses before touching anything.
//
// go-git's own Checkout is not used: it refuses any dirty tree outright, and
// when the tree is clean it resets the whole index, dropping staged changes.
func safeCheckout(r *git.Repository, branch plumbing.ReferenceName) error {
	wt, err := worktreeWithExcludes(r)
	if err != nil {
		return err
	}
	target, err := r.Reference(branch, true)
	if err != nil {
		return err
	}
	targetCommit, err := r.CommitObject(target.Hash())
	if err != nil {
		return err
	}
	targetTree, err := targetCommit.Tree()
	if err != nil {
		return err
	}
	var headTree *object.Tree
	if head, err := r.Head(); err == nil {
		if hc, err := r.CommitObject(head.Hash()); err == nil {
			headTree, _ = hc.Tree()
		}
	}
	changes, err := object.DiffTree(headTree, targetTree)
	if err != nil {
		return err
	}
	st, err := wt.Status()
	if err != nil {
		return err
	}
	var blocked []string
	for _, ch := range changes {
		if fs, ok := st[ch.From.Name]; ch.From.Name != "" && ok && isDirty(fs) {
			blocked = append(blocked, ch.From.Name)
		}
		if ch.To.Name == "" || ch.To.Name == ch.From.Name {
			continue
		}
		if fs, ok := st[ch.To.Name]; ok && isDirty(fs) {
			blocked = append(blocked, ch.To.Name)
		} else if ch.From.Name == "" {
			// A file the target adds must not already exist untracked —
			// status omits ignored files, so look at the disk too.
			if _, err := wt.Filesystem.Lstat(ch.To.Name); err == nil {
				blocked = append(blocked, ch.To.Name)
			}
		}
	}
	if len(blocked) > 0 {
		blocked = uniqueStrings(blocked)
		sort.Strings(blocked)
		return fmt.Errorf("your local changes to these files would be overwritten by checkout: %s — commit or stash them first", strings.Join(blocked, ", "))
	}

	idx, err := r.Storer.Index()
	if err != nil {
		return err
	}
	// Deletions first, so a directory the target replaces with a file (or
	// the reverse) is out of the way before anything is written.
	for _, ch := range changes {
		if ch.From.Name != "" && ch.From.Name != ch.To.Name {
			if err := removeWorktreeFile(wt.Filesystem, ch.From.Name); err != nil {
				return err
			}
			_, _ = idx.Remove(ch.From.Name)
		}
	}
	for _, ch := range changes {
		if ch.To.Name != "" {
			if err := writeWorktreeFile(r, wt.Filesystem, idx, ch.To.Name, ch.To.TreeEntry); err != nil {
				return err
			}
		}
	}
	if err := r.Storer.SetIndex(idx); err != nil {
		return err
	}
	return r.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branch))
}

func isDirty(fs *git.FileStatus) bool {
	return fs.Staging != git.Unmodified || fs.Worktree != git.Unmodified
}

// writeWorktreeFile materializes a tree entry on disk and records it in the
// index with the stat data git uses to skip rehashing unchanged files.
func writeWorktreeFile(r *git.Repository, fs billy.Filesystem, idx *index.Index, name string, e object.TreeEntry) error {
	if e.Mode == filemode.Submodule {
		// A submodule is a gitlink, not content; git leaves its directory alone.
		return setIndexEntry(idx, name, e, nil)
	}
	blob, err := r.BlobObject(e.Hash)
	if err != nil {
		return err
	}
	rd, err := blob.Reader()
	if err != nil {
		return err
	}
	defer func() { _ = rd.Close() }()
	if err := removeWorktreeFile(fs, name); err != nil {
		return err
	}
	if err := fs.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	if e.Mode == filemode.Symlink {
		target, err := io.ReadAll(rd)
		if err != nil {
			return err
		}
		if err := fs.Symlink(string(target), name); err != nil {
			return err
		}
	} else {
		osMode, err := e.Mode.ToOSFileMode()
		if err != nil {
			return err
		}
		f, err := fs.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, osMode.Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, rd); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	info, err := fs.Lstat(name)
	if err != nil {
		return err
	}
	return setIndexEntry(idx, name, e, info)
}

func setIndexEntry(idx *index.Index, name string, e object.TreeEntry, info os.FileInfo) error {
	_, _ = idx.Remove(name)
	entry := idx.Add(name)
	entry.Hash = e.Hash
	entry.Mode = e.Mode
	if info != nil {
		entry.ModifiedAt = info.ModTime()
		entry.Size = uint32(info.Size()) //nolint:gosec // the index stores sizes mod 2^32, as git does
	}
	return nil
}

// removeWorktreeFile deletes a tracked file and then any parent directories
// the deletion left empty, like git does on checkout.
func removeWorktreeFile(fs billy.Filesystem, name string) error {
	if err := fs.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for dir := path.Dir(name); dir != "." && dir != "/"; dir = path.Dir(dir) {
		entries, err := fs.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			break
		}
		if fs.Remove(dir) != nil {
			break
		}
	}
	return nil
}
