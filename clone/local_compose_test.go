package clone

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type composeService struct {
	Ports       []string          `yaml:"ports"`
	Volumes     []string          `yaml:"volumes"`
	Environment []string          `yaml:"environment"`
	Labels      map[string]string `yaml:"labels"`
}

type composeVolume struct {
	Labels map[string]string `yaml:"labels"`
}

type composeFile struct {
	Services map[string]composeService `yaml:"services"`
	Volumes  map[string]*composeVolume `yaml:"volumes"`
}

func testLocal(t *testing.T) *Local {
	return &Local{
		Name:      "shop",
		Dir:       t.TempDir(),
		CodeDir:   t.TempDir(),
		Port:      8320,
		Image:     "dockware/dev:latest",
		Redis:     true,
		Frontend:  &FrontendConfig{Path: "web", Port: 3000, Command: "npm run dev"},
		FrontPort: 3001,
	}
}

func renderCompose(t *testing.T, l *Local) composeFile {
	t.Helper()
	data, err := l.Compose()
	if err != nil {
		t.Fatal(err)
	}
	var f composeFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	return f
}

func TestComposeBindsLoopbackByDefault(t *testing.T) {
	l := testLocal(t)
	f := renderCompose(t, l)
	if got := f.Services["shop"].Ports; len(got) != 1 || got[0] != "127.0.0.1:8320:80" {
		t.Errorf("shop ports: got %v, want [127.0.0.1:8320:80]", got)
	}
	if got := f.Services["frontend"].Ports; len(got) != 1 || got[0] != "127.0.0.1:3001:3000" {
		t.Errorf("frontend ports: got %v, want [127.0.0.1:3001:3000]", got)
	}

	l.Bind = "0.0.0.0"
	if got := renderCompose(t, l).Services["shop"].Ports; len(got) != 1 || got[0] != "0.0.0.0:8320:80" {
		t.Errorf("--bind 0.0.0.0: got %v", got)
	}
	l.Bind = "::1"
	if got := renderCompose(t, l).Services["frontend"].Ports; len(got) != 1 || got[0] != "[::1]:3001:3000" {
		t.Errorf("IPv6 bind needs brackets: got %v", got)
	}
}

func TestComposeMountsSSHAgentOnlyWhenRequested(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/tmp/test-agent.sock")
	l := testLocal(t)
	hasAgent := func(s composeService) bool {
		for _, v := range s.Volumes {
			if strings.HasSuffix(v, ":"+agentSocket) {
				return true
			}
		}
		for _, e := range s.Environment {
			if strings.HasPrefix(e, "SSH_AUTH_SOCK=") {
				return true
			}
		}
		return false
	}
	if hasAgent(renderCompose(t, l).Services["shop"]) {
		t.Error("SSHAgent=false must neither mount the agent nor set SSH_AUTH_SOCK")
	}
	l.SSHAgent = true
	if !hasAgent(renderCompose(t, l).Services["shop"]) {
		t.Error("SSHAgent=true with a running agent must mount it")
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	if hasAgent(renderCompose(t, l).Services["shop"]) {
		t.Error("without a host agent there is nothing to mount")
	}
}

func TestComposeLabelsServicesAndVolumes(t *testing.T) {
	l := testLocal(t)
	f := renderCompose(t, l)
	for name, v := range f.Volumes {
		if v != nil {
			t.Errorf("volume %s without labels must stay null, got %+v", name, *v)
		}
	}

	withLabels := func(created, expires string) *Local {
		l := testLocal(t)
		l.Labels = map[string]string{
			"dockware.clone.source":  "user@host:/var/www/shop",
			"dockware.clone.created": created,
			"dockware.clone.expires": expires,
		}
		return l
	}
	l = withLabels("2026-11-01T10:00:00Z", "2026-11-15T10:00:00Z")
	f = renderCompose(t, l)
	for _, name := range []string{"vendor", "cache", "log", "theme", "bundles", "db", "var", "node_modules", "nuxt"} {
		v, ok := f.Volumes[name]
		if !ok || v == nil || v.Labels["dockware.clone.source"] != "user@host:/var/www/shop" {
			t.Errorf("volume %s: source label missing: %+v", name, v)
			continue
		}
		if len(v.Labels) != 1 {
			t.Errorf("volume %s must carry only the source label, got %v", name, v.Labels)
		}
	}
	for _, name := range []string{"shop", "redis", "frontend"} {
		if got := f.Services[name].Labels; got["dockware.clone.source"] != "user@host:/var/www/shop" ||
			got["dockware.clone.created"] != "2026-11-01T10:00:00Z" || got["dockware.clone.expires"] != "2026-11-15T10:00:00Z" {
			t.Errorf("service %s: labels missing: %v", name, got)
		}
	}

	// a rerun into the same out dir must not change the volume definitions
	volumesYAML := func(l *Local) string {
		data, err := l.Compose()
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Volumes map[string]any `yaml:"volumes"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		out, err := yaml.Marshal(doc.Volumes)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	first := volumesYAML(l)
	second := volumesYAML(withLabels("2026-11-17T08:00:00Z", "2026-12-01T08:00:00Z"))
	if first != second {
		t.Errorf("volume definitions differ between reruns:\n%s\n---\n%s", first, second)
	}
	if strings.Contains(first, "created") || strings.Contains(first, "expires") {
		t.Errorf("volumes carry created/expires labels:\n%s", first)
	}

	l = testLocal(t)
	l.Labels = map[string]string{"dockware.clone.created": "2026-11-01T10:00:00Z"}
	for name, v := range renderCompose(t, l).Volumes {
		if v != nil {
			t.Errorf("volume %s without source label must stay null, got %+v", name, *v)
		}
	}
}
