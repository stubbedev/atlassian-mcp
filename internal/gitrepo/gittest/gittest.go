package gittest

import (
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

type Repo struct {
	t    *testing.T
	Dir  string
	Repo *git.Repository
	Now  time.Time
}

func New(t *testing.T) *Repo {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // keep the user's global config and ignores out
	t.Setenv("XDG_CONFIG_HOME", "")
	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	return &Repo{t: t, Dir: dir, Repo: r, Now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (tr *Repo) Write(name, content string) {
	tr.t.Helper()
	p := filepath.Join(tr.Dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr *Repo) Read(name string) string {
	tr.t.Helper()
	b, err := os.ReadFile(filepath.Join(tr.Dir, name))
	if err != nil {
		tr.t.Fatal(err)
	}
	return string(b)
}

func (tr *Repo) Add(names ...string) {
	tr.t.Helper()
	wt, err := tr.Repo.Worktree()
	if err != nil {
		tr.t.Fatal(err)
	}
	for _, n := range names {
		if _, err := wt.Add(n); err != nil {
			tr.t.Fatal(err)
		}
	}
}

func (tr *Repo) Commit(msg string, author string) plumbing.Hash {
	tr.t.Helper()
	wt, err := tr.Repo.Worktree()
	if err != nil {
		tr.t.Fatal(err)
	}
	tr.Now = tr.Now.Add(time.Minute)
	sig := &object.Signature{Name: author, Email: strings.ToLower(author) + "@example.com", When: tr.Now}
	h, err := wt.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig, AllowEmptyCommits: true})
	if err != nil {
		tr.t.Fatal(err)
	}
	return h
}

// WithRemote creates a bare repository, adds it as origin, and pushes master.
func (tr *Repo) WithRemote() string {
	tr.t.Helper()
	bare := tr.t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		tr.t.Fatal(err)
	}
	if _, err := tr.Repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{bare}}); err != nil {
		tr.t.Fatal(err)
	}
	if err := tr.Repo.Push(&git.PushOptions{RemoteName: "origin", RefSpecs: []gitconfig.RefSpec{"refs/heads/master:refs/heads/master"}}); err != nil {
		tr.t.Fatal(err)
	}
	head, err := tr.Repo.Reference(plumbing.NewBranchReferenceName("master"), true)
	if err != nil {
		tr.t.Fatal(err)
	}
	if err := tr.Repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "master"), head.Hash())); err != nil {
		tr.t.Fatal(err)
	}
	cfg, err := tr.Repo.Config()
	if err != nil {
		tr.t.Fatal(err)
	}
	cfg.Branches["master"] = &gitconfig.Branch{Name: "master", Remote: "origin", Merge: plumbing.NewBranchReferenceName("master")}
	if err := tr.Repo.Storer.SetConfig(cfg); err != nil {
		tr.t.Fatal(err)
	}
	return bare
}
