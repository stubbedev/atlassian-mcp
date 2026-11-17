package gitrepo

import (
	"crypto/sha1"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stubbedev/atlassian-mcp/internal/gitrepo/gittest"
)

func TestGitBasics(t *testing.T) {
	tr := gittest.New(t)
	tr.Write("a.txt", "one\n")
	tr.Add("a.txt")
	tr.Commit("first commit\n\nbody", "Alice")
	tr.Write("a.txt", "one\ntwo\n")
	tr.Add("a.txt")
	second := tr.Commit("second commit", "Bob")

	if got := CurrentBranch(tr.Dir); got != "master" {
		t.Errorf("branch = %q", got)
	}
	// A subdirectory resolves to the enclosing repository, like git.
	if err := os.Mkdir(filepath.Join(tr.Dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !IsRepo(filepath.Join(tr.Dir, "sub")) || IsRepo(t.TempDir()) {
		t.Error("isGitRepo mismatch")
	}
	log := OnelineLog(mustOpen(t, tr.Dir), 5)
	want := second.String()[:shortHashLen] + " second commit\n"
	if !strings.HasPrefix(log, want) || !strings.HasSuffix(log, " first commit") {
		t.Errorf("log = %q", log)
	}
	if got := OnelineLog(mustOpen(t, tr.Dir), 1); strings.Contains(got, "\n") {
		t.Errorf("limit ignored: %q", got)
	}
	authors := RecentAuthors(mustOpen(t, tr.Dir), 10)
	if len(authors) != 2 || authors[0].Name != "Bob" || authors[1].Email != "alice@example.com" {
		t.Errorf("authors = %+v", authors)
	}
}

func TestGitMailmap(t *testing.T) {
	tr := gittest.New(t)
	tr.Write(".mailmap", "Alice Proper <alice@corp.com> <alice@example.com>\n")
	tr.Add(".mailmap")
	tr.Commit("c", "Alice")
	a := RecentAuthors(mustOpen(t, tr.Dir), 1)
	if len(a) != 1 || a[0].Name != "Alice Proper" || a[0].Email != "alice@corp.com" {
		t.Errorf("mailmap not applied: %+v", a)
	}
}

func TestGitStatusShort(t *testing.T) {
	tr := gittest.New(t)
	tr.Write("tracked.txt", "x\n")
	tr.Write("dir/kept.txt", "x\n")
	tr.Write(".gitignore", "*.log\n")
	tr.Add("tracked.txt", "dir/kept.txt", ".gitignore")
	tr.Commit("c", "Alice")

	tr.Write("tracked.txt", "changed\n")
	tr.Write("staged.txt", "new\n")
	tr.Add("staged.txt")
	tr.Write("newdir/deep/a.txt", "u\n")
	tr.Write("newdir/b.txt", "u\n")
	tr.Write("dir/untracked.txt", "u\n")
	tr.Write("noise.log", "ignored\n")

	got := mustStatus(t, tr.Dir)
	want := strings.Join([]string{
		"A  staged.txt",
		" M tracked.txt",
		"?? dir/untracked.txt",
		"?? newdir/",
	}, "\n")
	if got != want {
		t.Errorf("status:\n%s\nwant:\n%s", got, want)
	}
}

func TestGitGlobalExcludes(t *testing.T) {
	tr := gittest.New(t)
	tr.Write("a.txt", "x\n")
	tr.Add("a.txt")
	tr.Commit("c", "Alice")
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "git", "ignore"), []byte(".envrc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr.Write(".envrc", "secret\n")
	if got := mustStatus(t, tr.Dir); got != "" {
		t.Errorf("globally ignored file shown: %q", got)
	}
}

func TestGitWorktreeDiff(t *testing.T) {
	tr := gittest.New(t)
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = "\tline " + string(rune('a'+i))
	}
	lines[0] = "func main() {"
	tr.Write("f.go", strings.Join(lines, "\n")+"\n")
	tr.Write("gone.txt", "bye\n")
	tr.Add("f.go", "gone.txt")
	tr.Commit("c", "Alice")

	lines[10] = "CHANGED"
	tr.Write("f.go", strings.Join(lines, "\n")+"\n")
	if err := os.Remove(filepath.Join(tr.Dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	tr.Write("added.txt", "hi")
	tr.Add("added.txt")

	got, err := Diff(mustOpen(t, tr.Dir), "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"diff --git a/added.txt b/added.txt\nnew file mode 100644\nindex 0000000..",
		"--- /dev/null\n+++ b/added.txt\n@@ -0,0 +1 @@\n+hi\n\\ No newline at end of file\n",
		"diff --git a/f.go b/f.go\nindex ",
		" 100644\n--- a/f.go\n+++ b/f.go\n@@ -8,7 +8,7 @@ func main() {\n \tline h\n \tline i\n \tline j\n-\tline k\n+CHANGED\n \tline l\n",
		"diff --git a/gone.txt b/gone.txt\ndeleted file mode 100644\n",
		"--- a/gone.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diff missing %q in:\n%s", want, got)
		}
	}
	if stat := DiffStatSummary(mustOpen(t, tr.Dir)); stat != "3 files changed, 2 insertions(+), 2 deletions(-)" {
		t.Errorf("stat = %q", stat)
	}
}

func TestGitCommitDiffRenameAndRanges(t *testing.T) {
	tr := gittest.New(t)
	tr.Write("old.txt", strings.Repeat("same content line\n", 20))
	tr.Add("old.txt")
	base := tr.Commit("base", "Alice")
	if err := os.Rename(filepath.Join(tr.Dir, "old.txt"), filepath.Join(tr.Dir, "new.txt")); err != nil {
		t.Fatal(err)
	}
	wt, _ := tr.Repo.Worktree()
	if _, err := wt.Remove("old.txt"); err != nil {
		t.Fatal(err)
	}
	tr.Add("new.txt")
	tr.Commit("rename", "Alice")

	got, err := Diff(mustOpen(t, tr.Dir), base.String()[:8], "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "similarity index 100%\nrename from old.txt\nrename to new.txt\n") || strings.Contains(got, "@@") {
		t.Errorf("rename diff:\n%s", got)
	}
	dots, err := Diff(mustOpen(t, tr.Dir), "HEAD~1..HEAD", "")
	if err != nil || dots != got {
		t.Errorf("a..b differs from a b: %v\n%s", err, dots)
	}
	sym, err := Diff(mustOpen(t, tr.Dir), "HEAD~1...HEAD", "")
	if err != nil || sym != got {
		t.Errorf("a...b differs on linear history: %v\n%s", err, sym)
	}
}

func TestGitUpstreamAheadBehind(t *testing.T) {
	tr := gittest.New(t)
	tr.Write("a.txt", "1\n")
	tr.Add("a.txt")
	tr.Commit("c1", "Alice")
	tr.WithRemote()
	if err := setUpstream(tr.Repo, "master", "origin", "master"); err != nil {
		t.Fatal(err)
	}
	if got := UpstreamLine(mustOpen(t, tr.Dir)); got != "origin/master (up to date)" {
		t.Errorf("upstream = %q", got)
	}
	tr.Commit("local 1", "Alice")
	tr.Commit("local 2", "Alice")
	if got := UpstreamLine(mustOpen(t, tr.Dir)); got != "origin/master (2 ahead)" {
		t.Errorf("upstream = %q", got)
	}
	// Move origin/master to a commit HEAD lacks: diverged.
	head, _ := tr.Repo.Head()
	c, _ := tr.Repo.CommitObject(head.Hash())
	sig := object.Signature{Name: "R", Email: "r@x", When: tr.Now.Add(time.Hour)}
	remoteOnly := &object.Commit{Author: sig, Committer: sig, Message: "remote", TreeHash: c.TreeHash, ParentHashes: c.ParentHashes[:1]}
	obj := tr.Repo.Storer.NewEncodedObject()
	if err := remoteOnly.Encode(obj); err != nil {
		t.Fatal(err)
	}
	h, err := tr.Repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "master"), h)); err != nil {
		t.Fatal(err)
	}
	if got := UpstreamLine(mustOpen(t, tr.Dir)); got != "origin/master (1 ahead, 1 behind)" {
		t.Errorf("upstream = %q", got)
	}
	if _, err := resolveCommit(tr.Repo, "@{u}"); err != nil {
		t.Errorf("@{u}: %v", err)
	}
}

// git with feature.manyFiles writes the index with a zeroed checksum.
func TestGitSkipHashIndex(t *testing.T) {
	tr := gittest.New(t)
	tr.Write("a.txt", "1\n")
	tr.Add("a.txt")
	tr.Commit("c", "Alice")
	tr.Write("a.txt", "2\n")
	p := filepath.Join(tr.Dir, ".git", "index")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	copy(data[len(data)-sha1.Size:], make([]byte, sha1.Size))
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustStatus(t, tr.Dir); got != " M a.txt" {
		t.Errorf("status with skipHash index = %q", got)
	}
}

func TestApplyInsteadOf(t *testing.T) {
	rules := map[string]*gitconfig.URL{
		"ssh://git@host/": {Name: "ssh://git@host/", InsteadOf: "https://host/"},
		"x":               {Name: "x", InsteadOf: "https://"},
	}
	if got := applyInsteadOf("https://host/p/r.git", rules); got != "ssh://git@host/p/r.git" {
		t.Errorf("longest match not used: %q", got)
	}
	if got := applyInsteadOf("git@other:p/r.git", rules); got != "git@other:p/r.git" {
		t.Errorf("unmatched URL rewritten: %q", got)
	}
}

func TestNetrcCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("NETRC", "")
	if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte("machine a.example login u1 password p1\nmachine git.example\n  login u2\n  password p2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if u, p, ok := netrcCredential("git.example"); !ok || u != "u2" || p != "p2" {
		t.Errorf("got %q %q %v", u, p, ok)
	}
	if _, _, ok := netrcCredential("missing.example"); ok {
		t.Error("matched a missing host")
	}
}

func mustOpen(t *testing.T, dir string) *git.Repository {
	t.Helper()
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustStatus(t *testing.T, dir string) string {
	t.Helper()
	st, err := StatusShort(mustOpen(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestGitStatusShortTrackedChanges(t *testing.T) {
	tr := gittest.New(t)
	tr.Write("gone.txt", "x\n")
	tr.Write("unstaged-rm.txt", "x\n")
	tr.Write("exec.sh", "x\n")
	tr.Write("link", "x\n")
	tr.Write("touched.txt", "same\n")
	tr.Add("gone.txt", "unstaged-rm.txt", "exec.sh", "link", "touched.txt")
	tr.Commit("c", "Alice")

	wt, err := tr.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Remove("gone.txt"); err != nil {
		t.Fatal(err)
	}
	for _, step := range []error{
		os.Remove(filepath.Join(tr.Dir, "unstaged-rm.txt")),
		os.Chmod(filepath.Join(tr.Dir, "exec.sh"), 0o755),
		os.Remove(filepath.Join(tr.Dir, "link")),
		os.Symlink("exec.sh", filepath.Join(tr.Dir, "link")),
		os.Chtimes(filepath.Join(tr.Dir, "touched.txt"), tr.Now.Add(time.Hour), tr.Now.Add(time.Hour)),
	} {
		if step != nil {
			t.Fatal(step)
		}
	}

	want := strings.Join([]string{
		" M exec.sh",
		"D  gone.txt",
		" T link",
		" D unstaged-rm.txt",
	}, "\n")
	if got := mustStatus(t, tr.Dir); got != want {
		t.Errorf("status:\n%s\nwant:\n%s", got, want)
	}
}

func TestGitStatusShortIgnoreRules(t *testing.T) {
	tr := gittest.New(t)
	tr.Write(".gitignore", "vendor/\n")
	tr.Write("src/.gitignore", "*.gen\n")
	tr.Write("src/main.go", "x\n")
	tr.Add(".gitignore", "src/.gitignore", "src/main.go")
	tr.Commit("c", "Alice")

	tr.Write("vendor/pkg/a.php", "x\n")
	locked := filepath.Join(tr.Dir, "vendor", "pkg")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	tr.Write("src/out.gen", "x\n")
	tr.Write("onlyignored/.gitignore", "*\n")
	tr.Write("onlyignored/a.txt", "x\n")
	tr.Write("cache/x.tmp", "x\n")
	tr.Write("excluded.txt", "x\n")
	tr.Write(".git/info/exclude", "*.tmp\nexcluded.txt\n")
	tr.Write("emptyish/deeper/.keep", "x\n")
	tr.Write("nested/.git/HEAD", "ref: refs/heads/main\n")

	want := strings.Join([]string{
		"?? emptyish/",
		"?? nested/",
	}, "\n")
	if got := mustStatus(t, tr.Dir); got != want {
		t.Errorf("status:\n%s\nwant:\n%s", got, want)
	}
}

func TestTrackedStatusUsesCacheTree(t *testing.T) {
	tr := gittest.New(t)
	tr.Write("keep/a.txt", "x\n")
	tr.Write("edit/b.txt", "x\n")
	tr.Add("keep/a.txt", "edit/b.txt")
	head := tr.Commit("c", "Alice")
	tr.Write("edit/b.txt", "staged\n")
	tr.Add("edit/b.txt")

	r := mustOpen(t, tr.Dir)
	commit, err := r.CommitObject(head)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatal(err)
	}
	subtree := func(name string) plumbing.Hash {
		e, err := tree.FindEntry(name)
		if err != nil {
			t.Fatal(err)
		}
		return e.Hash
	}
	idx, err := r.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	idx.Cache = &index.Tree{Entries: []index.TreeEntry{
		{Path: "", Entries: -1, Trees: 2},
		{Path: "edit", Entries: -1},
		{Path: "keep", Entries: 1, Hash: subtree("keep")},
	}}
	ht, err := readHeadTree(r, idx)
	if err != nil {
		t.Fatal(err)
	}
	if _, read := ht.entries["keep/a.txt"]; read || !ht.unchanged["keep"] || ht.unchanged["edit"] {
		t.Errorf("only keep/ should be pruned: unchanged=%v entries=%v", ht.unchanged, ht.entries)
	}
	got, err := trackedStatus(r, tr.Dir, idx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]pathStatus{"edit/b.txt": {statusModified, statusUnmodified}}
	if !maps.Equal(got, want) {
		t.Errorf("status = %v, want %v", got, want)
	}

	idx.Cache = &index.Tree{Entries: []index.TreeEntry{{Path: "", Entries: 2, Trees: 0, Hash: commit.TreeHash}}}
	if got, err := trackedStatus(r, tr.Dir, idx); err != nil || len(got) != 0 {
		t.Errorf("a cache-tree root matching HEAD must report nothing staged: %v, %v", got, err)
	}

	idx.Cache = &index.Tree{Entries: []index.TreeEntry{{Path: "", Entries: -1, Trees: 1}}}
	if _, err := trackedStatus(r, tr.Dir, idx); err == nil {
		t.Error("a truncated cache-tree extension must be an error")
	}
}

func TestGitStatusShortSymlinkedParent(t *testing.T) {
	tr := gittest.New(t)
	tr.Write("dir/a.txt", "x\n")
	tr.Add("dir/a.txt")
	tr.Commit("c", "Alice")
	tr.Write("elsewhere/a.txt", "x\n")
	if err := os.RemoveAll(filepath.Join(tr.Dir, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(tr.Dir, "dir")); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		" D dir/a.txt",
		"?? dir",
		"?? elsewhere/",
	}, "\n")
	if got := mustStatus(t, tr.Dir); got != want {
		t.Errorf("status:\n%s\nwant:\n%s", got, want)
	}
}
