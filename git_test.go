package main

import (
	"crypto/sha1"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// The git layer must work with no git binary, so these tests build their
// repositories with go-git and never shell out.

type testRepo struct {
	t    *testing.T
	dir  string
	repo *git.Repository
	now  time.Time
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // keep the user's global config and ignores out
	t.Setenv("XDG_CONFIG_HOME", "")
	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	return &testRepo{t: t, dir: dir, repo: r, now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (tr *testRepo) write(name, content string) {
	tr.t.Helper()
	p := filepath.Join(tr.dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr *testRepo) read(name string) string {
	tr.t.Helper()
	b, err := os.ReadFile(filepath.Join(tr.dir, name))
	if err != nil {
		tr.t.Fatal(err)
	}
	return string(b)
}

func (tr *testRepo) add(names ...string) {
	tr.t.Helper()
	wt, err := tr.repo.Worktree()
	if err != nil {
		tr.t.Fatal(err)
	}
	for _, n := range names {
		if _, err := wt.Add(n); err != nil {
			tr.t.Fatal(err)
		}
	}
}

func (tr *testRepo) commit(msg string, author string) plumbing.Hash {
	tr.t.Helper()
	wt, err := tr.repo.Worktree()
	if err != nil {
		tr.t.Fatal(err)
	}
	tr.now = tr.now.Add(time.Minute)
	sig := &object.Signature{Name: author, Email: strings.ToLower(author) + "@example.com", When: tr.now}
	h, err := wt.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig, AllowEmptyCommits: true})
	if err != nil {
		tr.t.Fatal(err)
	}
	return h
}

// withRemote creates a bare repository, adds it as origin, and pushes master.
func (tr *testRepo) withRemote() string {
	tr.t.Helper()
	bare := tr.t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		tr.t.Fatal(err)
	}
	if _, err := tr.repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{bare}}); err != nil {
		tr.t.Fatal(err)
	}
	if err := pushBranch(tr.repo, "origin", "master"); err != nil {
		tr.t.Fatal(err)
	}
	return bare
}

func TestGitBasics(t *testing.T) {
	tr := newTestRepo(t)
	tr.write("a.txt", "one\n")
	tr.add("a.txt")
	tr.commit("first commit\n\nbody", "Alice")
	tr.write("a.txt", "one\ntwo\n")
	tr.add("a.txt")
	second := tr.commit("second commit", "Bob")

	if got := gitCurrentBranch(tr.dir); got != "master" {
		t.Errorf("branch = %q", got)
	}
	// A subdirectory resolves to the enclosing repository, like git.
	if err := os.Mkdir(filepath.Join(tr.dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isGitRepo(filepath.Join(tr.dir, "sub")) || isGitRepo(t.TempDir()) {
		t.Error("isGitRepo mismatch")
	}
	log := gitOnelineLog(tr.dir, 5)
	want := second.String()[:shortHashLen] + " second commit\n"
	if !strings.HasPrefix(log, want) || !strings.HasSuffix(log, " first commit") {
		t.Errorf("log = %q", log)
	}
	if got := gitOnelineLog(tr.dir, 1); strings.Contains(got, "\n") {
		t.Errorf("limit ignored: %q", got)
	}
	authors := gitRecentAuthors(tr.dir, 10)
	if len(authors) != 2 || authors[0].name != "Bob" || authors[1].email != "alice@example.com" {
		t.Errorf("authors = %+v", authors)
	}
}

func TestGitMailmap(t *testing.T) {
	tr := newTestRepo(t)
	tr.write(".mailmap", "Alice Proper <alice@corp.com> <alice@example.com>\n")
	tr.add(".mailmap")
	tr.commit("c", "Alice")
	a := gitRecentAuthors(tr.dir, 1)
	if len(a) != 1 || a[0].name != "Alice Proper" || a[0].email != "alice@corp.com" {
		t.Errorf("mailmap not applied: %+v", a)
	}
}

func TestGitStatusShort(t *testing.T) {
	tr := newTestRepo(t)
	tr.write("tracked.txt", "x\n")
	tr.write("dir/kept.txt", "x\n")
	tr.write(".gitignore", "*.log\n")
	tr.add("tracked.txt", "dir/kept.txt", ".gitignore")
	tr.commit("c", "Alice")

	tr.write("tracked.txt", "changed\n")
	tr.write("staged.txt", "new\n")
	tr.add("staged.txt")
	tr.write("newdir/deep/a.txt", "u\n")
	tr.write("newdir/b.txt", "u\n")
	tr.write("dir/untracked.txt", "u\n")
	tr.write("noise.log", "ignored\n")

	got := gitStatusShort(tr.dir)
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
	tr := newTestRepo(t)
	tr.write("a.txt", "x\n")
	tr.add("a.txt")
	tr.commit("c", "Alice")
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "git", "ignore"), []byte(".envrc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr.write(".envrc", "secret\n")
	if got := gitStatusShort(tr.dir); got != "" {
		t.Errorf("globally ignored file shown: %q", got)
	}
}

func TestGitWorktreeDiff(t *testing.T) {
	tr := newTestRepo(t)
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = "\tline " + string(rune('a'+i))
	}
	lines[0] = "func main() {"
	tr.write("f.go", strings.Join(lines, "\n")+"\n")
	tr.write("gone.txt", "bye\n")
	tr.add("f.go", "gone.txt")
	tr.commit("c", "Alice")

	lines[10] = "CHANGED"
	tr.write("f.go", strings.Join(lines, "\n")+"\n")
	if err := os.Remove(filepath.Join(tr.dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	tr.write("added.txt", "hi")
	tr.add("added.txt")

	got, err := gitDiff(tr.dir, "HEAD", "")
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
	if stat := gitDiffStatSummary(tr.dir); stat != "3 files changed, 2 insertions(+), 2 deletions(-)" {
		t.Errorf("stat = %q", stat)
	}
}

func TestGitCommitDiffRenameAndRanges(t *testing.T) {
	tr := newTestRepo(t)
	tr.write("old.txt", strings.Repeat("same content line\n", 20))
	tr.add("old.txt")
	base := tr.commit("base", "Alice")
	if err := os.Rename(filepath.Join(tr.dir, "old.txt"), filepath.Join(tr.dir, "new.txt")); err != nil {
		t.Fatal(err)
	}
	wt, _ := tr.repo.Worktree()
	if _, err := wt.Remove("old.txt"); err != nil {
		t.Fatal(err)
	}
	tr.add("new.txt")
	tr.commit("rename", "Alice")

	got, err := gitDiff(tr.dir, base.String()[:8], "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "similarity index 100%\nrename from old.txt\nrename to new.txt\n") || strings.Contains(got, "@@") {
		t.Errorf("rename diff:\n%s", got)
	}
	dots, err := gitDiff(tr.dir, "HEAD~1..HEAD", "")
	if err != nil || dots != got {
		t.Errorf("a..b differs from a b: %v\n%s", err, dots)
	}
	sym, err := gitDiff(tr.dir, "HEAD~1...HEAD", "")
	if err != nil || sym != got {
		t.Errorf("a...b differs on linear history: %v\n%s", err, sym)
	}
}

func TestGitUpstreamAheadBehind(t *testing.T) {
	tr := newTestRepo(t)
	tr.write("a.txt", "1\n")
	tr.add("a.txt")
	tr.commit("c1", "Alice")
	tr.withRemote()
	if err := setUpstream(tr.repo, "master", "origin", "master"); err != nil {
		t.Fatal(err)
	}
	if got := gitUpstreamLine(tr.dir); got != "origin/master (up to date)" {
		t.Errorf("upstream = %q", got)
	}
	tr.commit("local 1", "Alice")
	tr.commit("local 2", "Alice")
	if got := gitUpstreamLine(tr.dir); got != "origin/master (2 ahead)" {
		t.Errorf("upstream = %q", got)
	}
	// Move origin/master to a commit HEAD lacks: diverged.
	head, _ := tr.repo.Head()
	c, _ := tr.repo.CommitObject(head.Hash())
	sig := object.Signature{Name: "R", Email: "r@x", When: tr.now.Add(time.Hour)}
	remoteOnly := &object.Commit{Author: sig, Committer: sig, Message: "remote", TreeHash: c.TreeHash, ParentHashes: c.ParentHashes[:1]}
	obj := tr.repo.Storer.NewEncodedObject()
	if err := remoteOnly.Encode(obj); err != nil {
		t.Fatal(err)
	}
	h, err := tr.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "master"), h)); err != nil {
		t.Fatal(err)
	}
	if got := gitUpstreamLine(tr.dir); got != "origin/master (1 ahead, 1 behind)" {
		t.Errorf("upstream = %q", got)
	}
	if _, err := resolveCommit(tr.repo, "@{u}"); err != nil {
		t.Errorf("@{u}: %v", err)
	}
}

func TestGitBranchCheckoutAndPush(t *testing.T) {
	tr := newTestRepo(t)
	tr.write("shared.txt", "base\n")
	tr.write("other.txt", "base\n")
	tr.add("shared.txt", "other.txt")
	tr.commit("base", "Alice")
	bare := tr.withRemote()
	if err := tr.repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.NewRemoteHEADReferenceName("origin"), plumbing.NewRemoteReferenceName("origin", "master"))); err != nil {
		t.Fatal(err)
	}
	if got := getDefaultBranch(tr.dir); got != "master" {
		t.Errorf("default branch = %q", got)
	}

	// Unrelated local changes, staged and not, must survive branch creation.
	tr.write("other.txt", "unstaged edit\n")
	tr.write("staged.txt", "staged\n")
	tr.add("staged.txt")
	res := createBranch("feature/X-1", "", tr.dir, true)
	if text := res.Content[0].Text; !strings.Contains(text, "Pushed to origin/feature/X-1") {
		t.Fatalf("createBranch: %s", text)
	}
	if gitCurrentBranch(tr.dir) != "feature/X-1" {
		t.Fatal("not on new branch")
	}
	if tr.read("other.txt") != "unstaged edit\n" {
		t.Error("unstaged change lost")
	}
	if st := gitStatusShort(tr.dir); st != " M other.txt\nA  staged.txt" {
		t.Errorf("status after checkout = %q", st)
	}
	remote, _ := git.PlainOpen(bare)
	if _, err := remote.Reference(plumbing.NewBranchReferenceName("feature/X-1"), true); err != nil {
		t.Errorf("branch not pushed: %v", err)
	}
	if up := gitUpstreamLine(tr.dir); up != "origin/feature/X-1 (up to date)" {
		t.Errorf("upstream = %q", up)
	}

	// Commit a change to shared.txt on the branch, then go back to master:
	// a clean switch rewrites the file.
	wt, _ := tr.repo.Worktree()
	if _, err := wt.Remove("staged.txt"); err != nil {
		t.Fatal(err)
	}
	tr.write("shared.txt", "branch\n")
	tr.add("shared.txt")
	tr.commit("branch change", "Alice")
	if res := checkoutRemoteBranch("master", tr.dir); !strings.Contains(res.Content[0].Text, "Switched") {
		t.Fatalf("checkout master: %s", res.Content[0].Text)
	}
	if tr.read("shared.txt") != "base\n" || tr.read("other.txt") != "unstaged edit\n" {
		t.Error("checkout did not update the switched file or lost a local edit")
	}

	// A local edit to a file the switch would rewrite blocks it, untouched.
	tr.write("shared.txt", "conflicting edit\n")
	res = checkoutRemoteBranch("feature/X-1", tr.dir)
	if !strings.Contains(res.Content[0].Text, "would be overwritten") || !strings.Contains(res.Content[0].Text, "shared.txt") {
		t.Errorf("expected refusal, got %s", res.Content[0].Text)
	}
	if gitCurrentBranch(tr.dir) != "master" || tr.read("shared.txt") != "conflicting edit\n" {
		t.Error("refused checkout changed state")
	}

	info, err := checkRemoteBranch("feature/X-1", tr.dir)
	if err != nil || !info.exists || info.author == "" {
		t.Errorf("checkRemoteBranch = %+v, %v", info, err)
	}
	if info, _ := checkRemoteBranch("nope", tr.dir); info.exists {
		t.Error("missing branch reported as existing")
	}
}

func TestGitCheckoutTracksRemoteBranch(t *testing.T) {
	tr := newTestRepo(t)
	tr.write("a.txt", "1\n")
	tr.add("a.txt")
	tr.commit("c", "Alice")
	tr.withRemote()
	head, _ := tr.repo.Head()
	if err := tr.repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "topic"), head.Hash())); err != nil {
		t.Fatal(err)
	}
	res := checkoutRemoteBranch("topic", tr.dir)
	if !strings.Contains(res.Content[0].Text, "tracking origin/topic") {
		t.Fatalf("checkout: %s", res.Content[0].Text)
	}
	if up := gitUpstreamLine(tr.dir); up != "origin/topic (up to date)" {
		t.Errorf("upstream = %q", up)
	}
}

// git with feature.manyFiles writes the index with a zeroed checksum.
func TestGitSkipHashIndex(t *testing.T) {
	tr := newTestRepo(t)
	tr.write("a.txt", "1\n")
	tr.add("a.txt")
	tr.commit("c", "Alice")
	tr.write("a.txt", "2\n")
	p := filepath.Join(tr.dir, ".git", "index")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	copy(data[len(data)-sha1.Size:], make([]byte, sha1.Size))
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := gitStatusShort(tr.dir); got != " M a.txt" {
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
