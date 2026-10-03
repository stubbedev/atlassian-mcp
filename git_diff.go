package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	udiff "github.com/go-git/go-git/v5/utils/diff"
	dmp "github.com/sergi/go-diff/diffmatchpatch"
)

// gitDiff renders a unified diff the way `git diff` does for the same
// arguments: from..to between two commits when to is set, otherwise from
// against the working tree. from may be a symmetric range "a...b" (changes on
// b since it forked from a) or "a..b" (same as passing both ends).
func gitDiff(repoPath, from, to string) (string, error) {
	r, err := openRepo(repoPath)
	if err != nil {
		return "", err
	}
	if to == "" {
		if a, b, ok := strings.Cut(from, "..."); ok {
			return diffSymmetric(r, a, b)
		}
		if a, b, ok := strings.Cut(from, ".."); ok {
			from, to = a, b
		}
	}
	fromCommit, err := resolveCommit(r, orValue(from, "HEAD"))
	if err != nil {
		return "", err
	}
	if to == "" {
		diffs, err := worktreeDiffs(r, fromCommit)
		if err != nil {
			return "", err
		}
		return encodeDiffs(diffs), nil
	}
	toCommit, err := resolveCommit(r, orValue(to, "HEAD"))
	if err != nil {
		return "", err
	}
	return diffCommits(r, fromCommit, toCommit)
}

// diffSymmetric is `git diff a...b`: b against the merge base of a and b.
func diffSymmetric(r *git.Repository, a, b string) (string, error) {
	ac, err := resolveCommit(r, orValue(a, "HEAD"))
	if err != nil {
		return "", err
	}
	bc, err := resolveCommit(r, orValue(b, "HEAD"))
	if err != nil {
		return "", err
	}
	_, _, bases, err := compareCommits(r, ac.Hash, bc.Hash)
	if err != nil {
		return "", err
	}
	if len(bases) == 0 {
		return "", fmt.Errorf("%s and %s have no common ancestor", a, b)
	}
	base, err := r.CommitObject(bases[0])
	if err != nil {
		return "", err
	}
	return diffCommits(r, base, bc)
}

// diffCommits diffs two commits' trees, detecting renames as git does by default.
func diffCommits(r *git.Repository, from, to *object.Commit) (string, error) {
	ft, err := from.Tree()
	if err != nil {
		return "", err
	}
	tt, err := to.Tree()
	if err != nil {
		return "", err
	}
	changes, err := object.DiffTreeWithOptions(context.Background(), ft, tt, object.DefaultDiffTreeOptions)
	if err != nil {
		return "", err
	}
	var diffs []*fileDiff
	for _, ch := range changes {
		var fromSide, toSide *diffSide
		if ch.From.Name != "" {
			fromSide = &diffSide{path: ch.From.Name, hash: ch.From.TreeEntry.Hash, mode: ch.From.TreeEntry.Mode}
		}
		if ch.To.Name != "" {
			toSide = &diffSide{path: ch.To.Name, hash: ch.To.TreeEntry.Hash, mode: ch.To.TreeEntry.Mode}
		}
		oldContent, err := sideContent(r, fromSide)
		if err != nil {
			return "", err
		}
		newContent, err := sideContent(r, toSide)
		if err != nil {
			return "", err
		}
		diffs = append(diffs, newFileDiff(fromSide, toSide, oldContent, newContent))
	}
	sort.SliceStable(diffs, func(i, j int) bool { return diffs[i].sortPath() < diffs[j].sortPath() })
	return encodeDiffs(diffs), nil
}

func sideContent(r *git.Repository, s *diffSide) ([]byte, error) {
	if s == nil || s.mode == filemode.Submodule {
		return nil, nil
	}
	return blobBytes(r, s.hash)
}

// gitDiffStatSummary is the summary line of `git diff HEAD --stat`, e.g.
// "3 files changed, 10 insertions(+), 2 deletions(-)", or "" when clean.
func gitDiffStatSummary(repoPath string) string {
	r, err := openRepo(repoPath)
	if err != nil {
		return ""
	}
	head, err := resolveCommit(r, "HEAD")
	if err != nil {
		return ""
	}
	diffs, err := worktreeDiffs(r, head)
	if err != nil || len(diffs) == 0 {
		return ""
	}
	adds, dels := 0, 0
	for _, d := range diffs {
		for _, l := range d.lines {
			switch l.op {
			case '+':
				adds++
			case '-':
				dels++
			}
		}
	}
	parts := []string{pluralize(len(diffs), "file changed", "files changed")}
	if adds > 0 {
		parts = append(parts, pluralize(adds, "insertion(+)", "insertions(+)"))
	}
	if dels > 0 {
		parts = append(parts, pluralize(dels, "deletion(-)", "deletions(-)"))
	}
	return strings.Join(parts, ", ")
}

// ── commit against working tree ──────────────────────────────────────────────

// worktreeDiffs diffs a commit against the files on disk, over every path the
// commit or the index tracks — `git diff <commit>`. Untracked files are not
// part of it, as in git. A file whose index entry matches both the commit and
// the file's size and mtime is taken as unchanged without reading it.
func worktreeDiffs(r *git.Repository, c *object.Commit) ([]*fileDiff, error) {
	wt, err := r.Worktree()
	if err != nil {
		return nil, err
	}
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}
	idx, err := r.Storer.Index()
	if err != nil {
		return nil, err
	}
	old := map[string]object.TreeEntry{}
	walker := object.NewTreeWalker(tree, true, nil)
	defer walker.Close()
	for {
		name, e, err := walker.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if e.Mode != filemode.Dir {
			old[name] = e
		}
	}
	// A conflicted path has one entry per stage; any of them marks it tracked,
	// and the stage-0 entry (the resolved one) wins when present.
	indexed := map[string]*index.Entry{}
	for _, e := range idx.Entries {
		if _, seen := indexed[e.Name]; !seen || e.Stage == 0 {
			indexed[e.Name] = e
		}
	}
	paths := make([]string, 0, len(old)+len(indexed))
	for p := range old {
		paths = append(paths, p)
	}
	for p := range indexed {
		if _, ok := old[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	var diffs []*fileDiff
	for _, p := range paths {
		oe, inTree := old[p]
		ie := indexed[p]
		if oe.Mode == filemode.Submodule || (ie != nil && ie.Mode == filemode.Submodule) {
			continue
		}
		var from *diffSide
		if inTree {
			from = &diffSide{path: p, hash: oe.Hash, mode: oe.Mode}
		}
		to, content, err := worktreeSide(wt.Filesystem, p, ie, from)
		if err != nil {
			return nil, err
		}
		if from == nil && to == nil {
			continue
		}
		if from != nil && to != nil && from.hash == to.hash && from.mode == to.mode {
			continue
		}
		oldContent, err := sideContent(r, from)
		if err != nil {
			return nil, err
		}
		diffs = append(diffs, newFileDiff(from, to, oldContent, content))
	}
	return diffs, nil
}

// worktreeSide describes the file on disk at p, nil when it is gone or no
// longer tracked. content is nil when the file is unchanged from the index
// entry and that entry matches the commit.
func worktreeSide(fs billy.Filesystem, p string, ie *index.Entry, from *diffSide) (*diffSide, []byte, error) {
	if ie == nil {
		return nil, nil, nil
	}
	info, err := fs.Lstat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	mode := worktreeMode(info, ie.Mode)
	if from != nil && ie.Hash == from.hash && mode == ie.Mode &&
		uint32(info.Size()) == ie.Size && info.ModTime().Equal(ie.ModifiedAt) { //nolint:gosec // index sizes are mod 2^32
		return &diffSide{path: p, hash: ie.Hash, mode: mode}, nil, nil
	}
	var content []byte
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := fs.Readlink(p)
		if err != nil {
			return nil, nil, err
		}
		content = []byte(target)
	} else {
		f, err := fs.Open(p)
		if err != nil {
			return nil, nil, err
		}
		content, err = io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	return &diffSide{path: p, hash: plumbing.ComputeHash(plumbing.BlobObject, content), mode: mode}, content, nil
}

// worktreeMode maps a file on disk to a git mode. Windows has no executable
// bit, so there the index's mode stands, as with core.fileMode=false.
func worktreeMode(info os.FileInfo, indexMode filemode.FileMode) filemode.FileMode {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return filemode.Symlink
	case runtime.GOOS == "windows":
		return indexMode
	case info.Mode().Perm()&0o111 != 0:
		return filemode.Executable
	default:
		return filemode.Regular
	}
}

func blobBytes(r *git.Repository, h plumbing.Hash) ([]byte, error) {
	b, err := r.BlobObject(h)
	if err != nil {
		return nil, err
	}
	rd, err := b.Reader()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rd.Close() }()
	return io.ReadAll(rd)
}

// ── unified diff, in git's format ────────────────────────────────────────────

const diffContextLines = 3

type diffSide struct {
	path string
	hash plumbing.Hash
	mode filemode.FileMode
}

type diffLine struct {
	op    byte // ' ', '-' or '+'
	text  string
	noEOL bool // last line of its file, without a trailing newline
}

// fileDiff is one file's change; a nil side is an addition or deletion.
type fileDiff struct {
	from, to   *diffSide
	binary     bool
	similarity int // percent, for renames
	oldLines   []string
	lines      []diffLine
}

func (d *fileDiff) sortPath() string {
	if d.to != nil {
		return d.to.path
	}
	return d.from.path
}

// isBinaryContent applies git's heuristic: a NUL in the first 8000 bytes.
func isBinaryContent(b []byte) bool {
	return bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0
}

func newFileDiff(from, to *diffSide, oldContent, newContent []byte) *fileDiff {
	d := &fileDiff{from: from, to: to}
	if isBinaryContent(oldContent) || isBinaryContent(newContent) {
		d.binary = true
		if from != nil && to != nil && from.hash == to.hash {
			d.similarity = 100
		}
		return d
	}
	oldText, newText := string(oldContent), string(newContent)
	d.oldLines = strings.Split(oldText, "\n")
	same := 0
	for _, df := range udiff.Do(oldText, newText) {
		op := byte(' ')
		switch df.Type {
		case dmp.DiffDelete:
			op = '-'
		case dmp.DiffInsert:
			op = '+'
		default:
			same += len(df.Text)
		}
		text := df.Text
		for text != "" {
			line, rest, found := strings.Cut(text, "\n")
			d.lines = append(d.lines, diffLine{op: op, text: line, noEOL: !found})
			text = rest
		}
	}
	if size := max(len(oldText), len(newText)); size > 0 {
		d.similarity = same * 100 / size
	} else {
		d.similarity = 100
	}
	return d
}

func modeString(m filemode.FileMode) string { return fmt.Sprintf("%06o", uint32(m)) }

func abbrevHash(h plumbing.Hash) string { return h.String()[:7] }

func encodeDiffs(diffs []*fileDiff) string {
	var b strings.Builder
	for _, d := range diffs {
		d.encode(&b)
	}
	return b.String()
}

func (d *fileDiff) encode(b *strings.Builder) {
	fromPath, toPath := "", ""
	if d.from != nil {
		fromPath = d.from.path
	}
	if d.to != nil {
		toPath = d.to.path
	}
	aPath, bPath := orValue(fromPath, toPath), orValue(toPath, fromPath)
	fmt.Fprintf(b, "diff --git a/%s b/%s\n", aPath, bPath)

	switch {
	case d.from == nil:
		fmt.Fprintf(b, "new file mode %s\n", modeString(d.to.mode))
	case d.to == nil:
		fmt.Fprintf(b, "deleted file mode %s\n", modeString(d.from.mode))
	case d.from.mode != d.to.mode:
		fmt.Fprintf(b, "old mode %s\nnew mode %s\n", modeString(d.from.mode), modeString(d.to.mode))
	}
	if d.from != nil && d.to != nil && fromPath != toPath {
		fmt.Fprintf(b, "similarity index %d%%\nrename from %s\nrename to %s\n", d.similarity, fromPath, toPath)
	}
	if d.from != nil && d.to != nil && d.from.hash == d.to.hash {
		return // a pure rename or mode change has no content to show
	}
	var fromHash, toHash plumbing.Hash
	if d.from != nil {
		fromHash = d.from.hash
	}
	if d.to != nil {
		toHash = d.to.hash
	}
	fmt.Fprintf(b, "index %s..%s", abbrevHash(fromHash), abbrevHash(toHash))
	if d.from != nil && d.to != nil && d.from.mode == d.to.mode {
		fmt.Fprintf(b, " %s", modeString(d.to.mode))
	}
	b.WriteString("\n")

	aName, bName := "a/"+aPath, "b/"+bPath
	if d.from == nil {
		aName = "/dev/null"
	}
	if d.to == nil {
		bName = "/dev/null"
	}
	if d.binary {
		fmt.Fprintf(b, "Binary files %s and %s differ\n", aName, bName)
		return
	}
	if len(d.lines) == 0 {
		return // an empty file added or deleted
	}
	fmt.Fprintf(b, "--- %s\n+++ %s\n", aName, bName)
	d.encodeHunks(b)
}

// encodeHunks groups changed lines into hunks with three lines of context,
// merging hunks whose context would touch, as git does.
func (d *fileDiff) encodeHunks(b *strings.Builder) {
	n := len(d.lines)
	// oldBefore[i]/newBefore[i]: lines of each side consumed before line i.
	oldBefore, newBefore := make([]int, n+1), make([]int, n+1)
	for i, l := range d.lines {
		oldBefore[i+1], newBefore[i+1] = oldBefore[i], newBefore[i]
		if l.op != '+' {
			oldBefore[i+1]++
		}
		if l.op != '-' {
			newBefore[i+1]++
		}
	}
	for i := 0; i < n; {
		for i < n && d.lines[i].op == ' ' {
			i++
		}
		if i >= n {
			break
		}
		start := max(0, i-diffContextLines)
		j := i
		for {
			for j < n && d.lines[j].op != ' ' {
				j++
			}
			k := j
			for k < n && d.lines[k].op == ' ' {
				k++
			}
			if k < n && k-j <= 2*diffContextLines {
				j = k
				continue
			}
			break
		}
		end := min(n, j+diffContextLines)
		oldCount := oldBefore[end] - oldBefore[start]
		newCount := newBefore[end] - newBefore[start]
		fmt.Fprintf(b, "@@ -%s +%s @@", hunkRange(oldBefore[start], oldCount), hunkRange(newBefore[start], newCount))
		if fn := d.funcContext(oldBefore[start]); fn != "" {
			b.WriteString(" " + fn)
		}
		b.WriteString("\n")
		for _, l := range d.lines[start:end] {
			b.WriteByte(l.op)
			b.WriteString(l.text)
			b.WriteString("\n")
			if l.noEOL {
				b.WriteString("\\ No newline at end of file\n")
			}
		}
		i = end
	}
}

// hunkRange formats one side of a hunk header: "start,count", with ",1"
// omitted, and an empty side placed after the line it follows.
func hunkRange(before, count int) string {
	switch count {
	case 0:
		return fmt.Sprintf("%d,0", before)
	case 1:
		return strconv.Itoa(before + 1)
	}
	return fmt.Sprintf("%d,%d", before+1, count)
}

// funcContext finds the hunk header's trailing context the way git's default
// funcname rule does: the nearest line above the hunk that starts with a
// letter, '_' or '$', cut to 80 bytes.
func (d *fileDiff) funcContext(firstOldLine int) string {
	for i := min(firstOldLine, len(d.oldLines)) - 1; i >= 0; i-- {
		line := d.oldLines[i]
		if line == "" {
			continue
		}
		c := line[0]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '$' {
			line = strings.TrimRight(line, " \t\r")
			if len(line) > 80 {
				line = line[:80]
			}
			return line
		}
	}
	return ""
}
