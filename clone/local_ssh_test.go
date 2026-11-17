package clone

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNeedsSSHAgent(t *testing.T) {
	cases := []struct {
		name     string
		manifest string // composer.json, "" = none
		lock     string // composer.lock, "" = none
		want     bool
	}{
		{"no composer files", "", "", false},
		{"https and disabled packagist", `{"repositories": [{"type": "composer", "url": "https://packages.shopware.com"}, {"packagist.org": false}]}`, "", false},
		{"scp-like vcs repository", `{"repositories": [{"type": "vcs", "url": "git@github.com:acme/plugin.git"}]}`, "", true},
		{"repositories keyed by name", `{"repositories": {"acme": {"type": "vcs", "url": "ssh://git@gitlab.example.com:2222/acme/plugin.git"}}}`, "", true},
		{"git+ssh", `{"repositories": [{"type": "git", "url": "git+ssh://git@bitbucket.org/acme/plugin.git"}]}`, "", true},
		{"https with user info is no ssh", `{"repositories": [{"type": "vcs", "url": "https://token@github.com/acme/plugin.git"}]}`, "", false},
		{"locked package from ssh source", "", `{"packages": [{"name": "acme/plugin", "source": {"type": "git", "url": "git@github.com:acme/plugin.git"}, "dist": {"type": "zip", "url": "https://api.github.com/repos/acme/plugin/zipball/1"}}]}`, true},
		{"locked dev package from ssh dist", "", `{"packages-dev": [{"name": "acme/tool", "dist": {"type": "git", "url": "ssh://git@example.com/acme/tool.git"}}]}`, true},
		{"locked https only", `{}`, `{"packages": [{"name": "shopware/core", "source": {"url": "https://github.com/shopware/core.git"}, "dist": {"url": "https://api.github.com/x"}}]}`, false},
		{"broken json", `{`, `{`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for file, content := range map[string]string{"composer.json": c.manifest, "composer.lock": c.lock} {
				if content == "" {
					continue
				}
				if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := NeedsSSHAgent(dir); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestEngineMismatch(t *testing.T) {
	for _, v := range []string{"8.0.36", "8.4.3", "5.7.44-log", ""} {
		if got := EngineMismatch(v); got != "" {
			t.Errorf("%q is MySQL: got %q", v, got)
		}
	}
	for _, v := range []string{"10.11.6-MariaDB-0+deb12u1", "5.5.5-10.6.12-mariadb"} {
		got := EngineMismatch(v)
		if got == "" || !strings.Contains(got, "--strategy shadow") {
			t.Errorf("%q is MariaDB: got %q, want a warning that names --strategy shadow", v, got)
		}
	}
}
