package main

import (
	"io"
	"strings"

	"github.com/go-git/go-git/v5"
)

// mailmap applies a repository's .mailmap, which git uses for %aN/%aE so one
// person committing under several names or addresses is counted once.
type mailmap struct {
	byEmail     map[string]mailmapIdentity // commit email → identity
	byNameEmail map[string]mailmapIdentity // commit name + "\x00" + email → identity
}

type mailmapIdentity struct{ name, email string }

func (m mailmap) resolve(name, email string) (string, string) {
	key := strings.ToLower(email)
	id, ok := m.byNameEmail[strings.ToLower(name)+"\x00"+key]
	if !ok {
		id, ok = m.byEmail[key]
	}
	if !ok {
		return name, email
	}
	if id.name != "" {
		name = id.name
	}
	if id.email != "" {
		email = id.email
	}
	return name, email
}

// loadMailmap reads .mailmap from HEAD's tree, as git does in a bare or
// sparse checkout, falling back to the working tree copy.
func loadMailmap(r *git.Repository) mailmap {
	m := mailmap{byEmail: map[string]mailmapIdentity{}, byNameEmail: map[string]mailmapIdentity{}}
	var data string
	if wt, err := r.Worktree(); err == nil {
		if f, err := wt.Filesystem.Open(".mailmap"); err == nil {
			b, _ := io.ReadAll(f)
			_ = f.Close()
			data = string(b)
		}
	}
	if data == "" {
		if head, err := resolveCommit(r, "HEAD"); err == nil {
			if f, err := head.File(".mailmap"); err == nil {
				data, _ = f.Contents()
			}
		}
	}
	for line := range strings.SplitSeq(data, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		m.add(line)
	}
	return m
}

// add parses one entry: "Proper Name <proper@email> [Commit Name] <commit@email>",
// where either proper part may be omitted.
func (m mailmap) add(line string) {
	type part struct{ name, email string }
	var parts []part
	rest := line
	for {
		open := strings.Index(rest, "<")
		if open < 0 {
			break
		}
		closeIdx := strings.Index(rest[open:], ">")
		if closeIdx < 0 {
			break
		}
		parts = append(parts, part{strings.TrimSpace(rest[:open]), strings.TrimSpace(rest[open+1 : open+closeIdx])})
		rest = rest[open+closeIdx+1:]
	}
	switch len(parts) {
	case 1:
		// "Proper Name <commit@email>"
		if parts[0].name != "" {
			m.byEmail[strings.ToLower(parts[0].email)] = mailmapIdentity{name: parts[0].name}
		}
	case 2:
		id := mailmapIdentity{name: parts[0].name, email: parts[0].email}
		email := strings.ToLower(parts[1].email)
		if parts[1].name != "" {
			m.byNameEmail[strings.ToLower(parts[1].name)+"\x00"+email] = id
		} else {
			m.byEmail[email] = id
		}
	}
}
