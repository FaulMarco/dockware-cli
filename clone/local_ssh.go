package clone

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
)

const agentSocket = "/run/ssh-agent.sock"

// hostSSHAgent returns the host path of the ssh-agent socket that can be
// bind-mounted into a container, or "" when no agent is running.
func hostSSHAgent() string {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return ""
	}
	if runtime.GOOS == "darwin" {
		// Docker Desktop cannot mount launchd sockets; it proxies the agent here
		return "/run/host-services/ssh-auth.sock"
	}
	return sock
}

// sshURLRe matches git URLs that authenticate with ssh keys: ssh://,
// git+ssh:// and the scp-like user@host:path.
var sshURLRe = regexp.MustCompile(`^(?:(?:git\+)?ssh://|[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:)`)

// NeedsSSHAgent reports whether composer needs ssh keys for the checkout in
// codeDir: a repositories[].url of composer.json or a source.url / dist.url
// of a composer.lock package uses ssh. Unreadable files count as no.
func NeedsSSHAgent(codeDir string) bool {
	var manifest struct {
		Repositories json.RawMessage `json:"repositories"`
	}
	if data, err := os.ReadFile(filepath.Join(codeDir, "composer.json")); err == nil && json.Unmarshal(data, &manifest) == nil {
		// a list of repositories or an object keyed by name; entries such
		// as {"packagist.org": false} have no url
		var list []json.RawMessage
		if json.Unmarshal(manifest.Repositories, &list) != nil {
			var byName map[string]json.RawMessage
			_ = json.Unmarshal(manifest.Repositories, &byName)
			for _, r := range byName {
				list = append(list, r)
			}
		}
		for _, raw := range list {
			var repo struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(raw, &repo) == nil && sshURLRe.MatchString(repo.URL) {
				return true
			}
		}
	}
	type location struct {
		URL string `json:"url"`
	}
	type lockPackage struct {
		Source location `json:"source"`
		Dist   location `json:"dist"`
	}
	var lock struct {
		Packages    []lockPackage `json:"packages"`
		PackagesDev []lockPackage `json:"packages-dev"`
	}
	data, err := os.ReadFile(filepath.Join(codeDir, "composer.lock"))
	if err != nil || json.Unmarshal(data, &lock) != nil {
		return false
	}
	for _, p := range append(lock.Packages, lock.PackagesDev...) {
		if sshURLRe.MatchString(p.Source.URL) || sshURLRe.MatchString(p.Dist.URL) {
			return true
		}
	}
	return false
}

// stopMySQLScript shuts the container's MySQL down cleanly and waits up to
// two minutes for mysqld to exit; docker stop's grace period is too short to
// flush a large buffer pool.
const stopMySQLScript = localMySQLAdmin + ` shutdown || exit 1
alive() { pgrep -x mysqld >/dev/null 2>&1 || pidof mysqld >/dev/null 2>&1; }
i=0
while alive; do
	i=$((i + 1))
	if [ "$i" -gt 120 ]; then echo "mysqld still running after 120 s"; exit 1; fi
	sleep 1
done
`

const localMySQLAdmin = "MYSQL_PWD=root mysqladmin -h127.0.0.1 -uroot"

// DropSSHAgent takes the host's ssh-agent away from the shop container once
// composer is done with it: the compose file is rewritten without the agent
// and only the shop service is recreated, so the running clone holds no key
// access. Call it after InstallVendor and Import have both finished. MySQL
// is shut down cleanly first; its data and the SET PERSIST tuning live in the
// db volume and survive. What the old container kept in its own filesystem,
// the PHP extensions EnsurePHPExtensions added and an nvm-installed node, is
// installed again.
func (l *Local) DropSSHAgent(ctx context.Context, log io.Writer) error {
	if !l.SSHAgent {
		return nil
	}
	l.SSHAgent = false
	if err := l.WriteFiles(); err != nil {
		return err
	}
	if hostSSHAgent() == "" {
		// the compose file never mounted an agent: nothing to recreate
		return nil
	}
	fmt.Fprintf(log, "  ssh-agent: recreating %s without the agent\n", l.container())
	if out, err := l.execRoot(ctx, stopMySQLScript); err != nil {
		fmt.Fprintf(log, "  ssh-agent: stopping MySQL cleanly failed, InnoDB recovers on start: %s\n", tailLines(out, 1))
	}
	if err := l.compose(ctx, StepDocker, "up", "-d", "--no-deps", "--force-recreate", "shop"); err != nil {
		return err
	}
	if err := l.started(ctx); err != nil {
		return err
	}
	if _, err := l.EnsurePHPExtensions(ctx, l.phpExts, log); err != nil {
		return err
	}
	if l.NodeVersion != "" {
		if _, err := l.UseNode(ctx, log); err != nil {
			return err
		}
	}
	return nil
}
