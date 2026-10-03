package main

import (
	"bufio"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitserver "github.com/go-git/go-git/v5/plumbing/transport/server"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/kevinburke/ssh_config"
	"golang.org/x/crypto/ssh"
)

// Credentials for fetch/push, found where the git CLI would find them, minus
// anything that needs another program (credential helpers, askpass):
//
//   - ssh: keys from a running ssh-agent, IdentityFile entries in ~/.ssh/config,
//     and the default ~/.ssh/id_* keys that have no passphrase. Host keys are
//     checked against ~/.ssh/known_hosts; HostName and Port from ~/.ssh/config
//     are honored.
//   - http(s): credentials in the URL, ~/.git-credentials (the "store" helper's
//     file), ~/.netrc, and — for the configured Bitbucket instance only — the
//     Bitbucket token this server already holds.

// Local remotes (a path or file:// URL) would otherwise run git-upload-pack
// and git-receive-pack; serve them in-process instead.
func init() {
	gitclient.InstallProtocol("file", gitserver.NewClient(gitserver.NewFilesystemLoader(osfs.New(""))))
}

func netContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), gitNetworkTimeout)
}

// gitAuth picks the credentials for a remote URL; nil lets go-git go without.
func gitAuth(remote string) transport.AuthMethod {
	ep, err := transport.NewEndpoint(remote)
	if err != nil {
		return nil
	}
	switch ep.Protocol {
	case "ssh":
		return sshAuth(ep)
	case "http", "https":
		return httpAuth(ep)
	}
	return nil
}

func sshAuth(ep *transport.Endpoint) transport.AuthMethod {
	user := ep.User
	if user == "" {
		user = ssh_config.Get(ep.Host, "User")
	}
	if user == "" {
		user = "git"
	}
	var signers []ssh.Signer
	seen := map[string]bool{}
	addSigner := func(s ssh.Signer) {
		key := string(s.PublicKey().Marshal())
		if !seen[key] {
			seen[key] = true
			signers = append(signers, s)
		}
	}
	// Keys named for this host first: servers cap auth attempts, and an agent
	// holding many keys could use them up before the right one is offered.
	keyFiles := ssh_config.GetAll(ep.Host, "IdentityFile")
	for _, f := range keyFiles {
		if s := loadKeyFile(f); s != nil {
			addSigner(s)
		}
	}
	if agent, err := gitssh.NewSSHAgentAuth(user); err == nil {
		if agentSigners, err := agent.Callback(); err == nil {
			for _, s := range agentSigners {
				addSigner(s)
			}
		}
	}
	for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		if s := loadKeyFile(filepath.Join("~", ".ssh", name)); s != nil {
			addSigner(s)
		}
	}
	return &gitssh.PublicKeysCallback{
		User:     user,
		Callback: func() ([]ssh.Signer, error) { return signers, nil },
	}
}

// loadKeyFile reads an unencrypted private key; nil when it is missing or
// needs a passphrase this process has no way to ask for.
func loadKeyFile(path string) ssh.Signer {
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return nil
	}
	s, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil
	}
	return s
}

func httpAuth(ep *transport.Endpoint) transport.AuthMethod {
	if ep.User != "" && ep.Password != "" {
		return &githttp.BasicAuth{Username: ep.User, Password: ep.Password}
	}
	if user, pass, ok := storedCredential(ep); ok {
		return &githttp.BasicAuth{Username: user, Password: pass}
	}
	if user, pass, ok := netrcCredential(ep.Host); ok {
		return &githttp.BasicAuth{Username: user, Password: pass}
	}
	if bitbucket != nil && bitbucket.token != "" {
		if base, err := url.Parse(bitbucket.baseURL); err == nil &&
			strings.EqualFold(base.Scheme, ep.Protocol) && strings.EqualFold(base.Hostname(), ep.Host) {
			return &githttp.TokenAuth{Token: bitbucket.token}
		}
	}
	return nil
}

// storedCredential looks the endpoint up in git's credential-store files.
func storedCredential(ep *transport.Endpoint) (user, pass string, ok bool) {
	var files []string
	if home, err := os.UserHomeDir(); err == nil {
		files = append(files, filepath.Join(home, ".git-credentials"))
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		files = append(files, filepath.Join(xdg, "git", "credentials"))
	} else if home, err := os.UserHomeDir(); err == nil {
		files = append(files, filepath.Join(home, ".config", "git", "credentials"))
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			u, err := url.Parse(strings.TrimSpace(line))
			if err != nil || u.User == nil {
				continue
			}
			p, hasPass := u.User.Password()
			if !hasPass || !strings.EqualFold(u.Scheme, ep.Protocol) || !strings.EqualFold(u.Hostname(), ep.Host) {
				continue
			}
			if port := u.Port(); port != "" && port != portString(ep.Port) {
				continue
			}
			if path := strings.Trim(u.Path, "/"); path != "" && !strings.HasPrefix(strings.Trim(ep.Path, "/"), path) {
				continue
			}
			return u.User.Username(), p, true
		}
	}
	return "", "", false
}

func portString(p int) string {
	if p == 0 {
		return ""
	}
	return strconv.Itoa(p)
}

// netrcCredential reads a machine's login/password from ~/.netrc (_netrc on
// Windows), the file curl — and so git over http — consults.
func netrcCredential(host string) (user, pass string, ok bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", false
	}
	path := os.Getenv("NETRC")
	if path == "" {
		path = filepath.Join(home, ".netrc")
		if !fileExists(path) {
			path = filepath.Join(home, "_netrc")
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Split(bufio.ScanWords)
	var machine, login, password string
	inDefault := false
	flush := func() bool {
		return (inDefault || strings.EqualFold(machine, host)) && login != "" && password != ""
	}
	for sc.Scan() {
		switch tok := sc.Text(); tok {
		case "machine", "default":
			if flush() {
				return login, password, true
			}
			machine, login, password, inDefault = "", "", "", tok == "default"
			if tok == "machine" && sc.Scan() {
				machine = sc.Text()
			}
		case "login":
			if sc.Scan() {
				login = sc.Text()
			}
		case "password":
			if sc.Scan() {
				password = sc.Text()
			}
		}
	}
	if flush() {
		return login, password, true
	}
	return "", "", false
}
