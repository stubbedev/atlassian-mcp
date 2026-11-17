package server

import (
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stubbedev/atlassian-mcp/internal/gitrepo"
	"github.com/stubbedev/atlassian-mcp/internal/gitrepo/gittest"
)

func TestGitBranchCheckoutAndPush(t *testing.T) {
	tr := gittest.New(t)
	tr.Write("shared.txt", "base\n")
	tr.Write("other.txt", "base\n")
	tr.Add("shared.txt", "other.txt")
	tr.Commit("base", "Alice")
	bare := tr.WithRemote()
	if err := tr.Repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.NewRemoteHEADReferenceName("origin"), plumbing.NewRemoteReferenceName("origin", "master"))); err != nil {
		t.Fatal(err)
	}
	if got := getDefaultBranch(mustOpen(t, tr.Dir)); got != "master" {
		t.Errorf("default branch = %q", got)
	}

	// Unrelated local changes, staged and not, must survive branch creation.
	tr.Write("other.txt", "unstaged edit\n")
	tr.Write("staged.txt", "staged\n")
	tr.Add("staged.txt")
	res := createBranch("feature/X-1", "", tr.Dir, true)
	if text := res.Content[0].Text; !strings.Contains(text, "Pushed to origin/feature/X-1") {
		t.Fatalf("createBranch: %s", text)
	}
	if gitrepo.CurrentBranch(tr.Dir) != "feature/X-1" {
		t.Fatal("not on new branch")
	}
	if tr.Read("other.txt") != "unstaged edit\n" {
		t.Error("unstaged change lost")
	}
	if st := mustStatus(t, tr.Dir); st != " M other.txt\nA  staged.txt" {
		t.Errorf("status after checkout = %q", st)
	}
	remote, _ := git.PlainOpen(bare)
	if _, err := remote.Reference(plumbing.NewBranchReferenceName("feature/X-1"), true); err != nil {
		t.Errorf("branch not pushed: %v", err)
	}
	if up := gitrepo.UpstreamLine(mustOpen(t, tr.Dir)); up != "origin/feature/X-1 (up to date)" {
		t.Errorf("upstream = %q", up)
	}

	// Commit a change to shared.txt on the branch, then go back to master:
	// a clean switch rewrites the file.
	wt, _ := tr.Repo.Worktree()
	if _, err := wt.Remove("staged.txt"); err != nil {
		t.Fatal(err)
	}
	tr.Write("shared.txt", "branch\n")
	tr.Add("shared.txt")
	tr.Commit("branch change", "Alice")
	if res := checkoutRemoteBranch("master", tr.Dir); !strings.Contains(res.Content[0].Text, "Switched") {
		t.Fatalf("checkout master: %s", res.Content[0].Text)
	}
	if tr.Read("shared.txt") != "base\n" || tr.Read("other.txt") != "unstaged edit\n" {
		t.Error("checkout did not update the switched file or lost a local edit")
	}

	// A local edit to a file the switch would rewrite blocks it, untouched.
	tr.Write("shared.txt", "conflicting edit\n")
	res = checkoutRemoteBranch("feature/X-1", tr.Dir)
	if !strings.Contains(res.Content[0].Text, "would be overwritten") || !strings.Contains(res.Content[0].Text, "shared.txt") {
		t.Errorf("expected refusal, got %s", res.Content[0].Text)
	}
	if gitrepo.CurrentBranch(tr.Dir) != "master" || tr.Read("shared.txt") != "conflicting edit\n" {
		t.Error("refused checkout changed state")
	}

	info, err := gitrepo.CheckRemoteBranch("feature/X-1", tr.Dir, nil)
	if err != nil || !info.Exists || info.Author == "" {
		t.Errorf("checkRemoteBranch = %+v, %v", info, err)
	}
	if info, _ := gitrepo.CheckRemoteBranch("nope", tr.Dir, nil); info.Exists {
		t.Error("missing branch reported as existing")
	}
}

func TestGitCheckoutTracksRemoteBranch(t *testing.T) {
	tr := gittest.New(t)
	tr.Write("a.txt", "1\n")
	tr.Add("a.txt")
	tr.Commit("c", "Alice")
	tr.WithRemote()
	head, _ := tr.Repo.Head()
	if err := tr.Repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "topic"), head.Hash())); err != nil {
		t.Fatal(err)
	}
	res := checkoutRemoteBranch("topic", tr.Dir)
	if !strings.Contains(res.Content[0].Text, "tracking origin/topic") {
		t.Fatalf("checkout: %s", res.Content[0].Text)
	}
	if up := gitrepo.UpstreamLine(mustOpen(t, tr.Dir)); up != "origin/topic (up to date)" {
		t.Errorf("upstream = %q", up)
	}
}

func mustOpen(t *testing.T, dir string) *git.Repository {
	t.Helper()
	r, err := gitrepo.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustStatus(t *testing.T, dir string) string {
	t.Helper()
	st, err := gitrepo.StatusShort(mustOpen(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	return st
}
