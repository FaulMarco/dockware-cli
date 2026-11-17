package clone

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// phpImageLabel marks the shop images PrepareImage builds; its value is the base image.
const phpImageLabel = "dockware.clone.php-image"

// PrepareImage switches l.Image to an image with the apt packages of the
// server's PHP extensions, built once per base image and package set and
// reused by later clones. Without it every container start installs them
// from the PPA again, also the one DropSSHAgent recreates. Any failure keeps
// l.Image as it is: EnsurePHPExtensions then installs them in the container.
func (l *Local) PrepareImage(ctx context.Context, want []string, log io.Writer) {
	ver := phpMajorMinor(l.PHPVersion)
	if ver == "" || len(want) == 0 {
		return
	}
	fail := func(what string, err error, out string) {
		fmt.Fprintf(log, "  php image: %s failed (%v), the extensions are installed in the container\n", what, err)
		if s := strings.TrimSpace(tailLines(out, 10)); s != "" {
			fmt.Fprintf(log, "    %s\n", strings.ReplaceAll(s, "\n", "\n    "))
		}
	}
	base := l.Image
	baseID, err := dockerInspect(ctx, base, "{{.Id}}")
	if err != nil {
		if out, perr := exec.CommandContext(ctx, "docker", "pull", base).CombinedOutput(); perr != nil {
			fail("docker pull "+base, perr, string(out))
			return
		}
		if baseID, err = dockerInspect(ctx, base, "{{.Id}}"); err != nil {
			fail("docker image inspect "+base, err, "")
			return
		}
	}
	have, out, err := imagePHPModules(ctx, base, ver)
	if err != nil {
		fail("php"+ver+" -m in "+base, err, out)
		return
	}
	pkgs := phpExtPackages(ver, want, have)
	if len(pkgs) == 0 {
		return
	}
	names := make([]string, len(pkgs))
	for i, p := range pkgs {
		names[i] = p.Name
	}
	sum := sha256.Sum256([]byte(baseID + "\n" + strings.Join(names, " ")))
	tag := "dockware-clone-shop:" + hex.EncodeToString(sum[:])[:12]
	if _, err := dockerInspect(ctx, tag, "{{.Id}}"); err == nil {
		fmt.Fprintf(log, "  php image: %s (cached)\n", tag)
	} else {
		// the digest pins the base the tag was hashed from; a plain tag
		// could resolve to a newer pull from the registry
		from := base
		if d, err := dockerInspect(ctx, base, `{{if .RepoDigests}}{{index .RepoDigests 0}}{{end}}`); err == nil && d != "" {
			from = d
		}
		user, _ := dockerInspect(ctx, base, "{{.Config.User}}")
		fmt.Fprintf(log, "  php image: building %s once (%s)\n", tag, strings.Join(names, ", "))
		t := time.Now()
		cmd := exec.CommandContext(ctx, "docker", "build", "-q", "--label", phpImageLabel+"="+base, "-t", tag, "-")
		cmd.Stdin = strings.NewReader(phpImageDockerfile(from, user, names))
		if out, err := cmd.CombinedOutput(); err != nil {
			fail("docker build", err, string(out))
			return
		}
		fmt.Fprintf(log, "  php image: built in %s\n", time.Since(t).Round(time.Second))
	}
	if have, out, err = imagePHPModules(ctx, tag, ver); err != nil {
		fail("php"+ver+" -m in "+tag, err, out)
		return
	}
	l.Image = tag
	// the build fails when a package with a candidate does not install, so
	// what is still missing has no apt package
	l.phpNoPackage = map[string]bool{}
	var none []string
	for _, p := range phpExtPackages(ver, want, have) {
		l.phpNoPackage[p.Name] = true
		none = append(none, p.Name)
	}
	if len(none) > 0 {
		fmt.Fprintf(log, "  php extensions: no apt package %s\n", strings.Join(none, ", "))
	}
}

// phpImageDockerfile installs the packages of names that apt has a candidate
// for; one that has a candidate but does not install fails the build.
func phpImageDockerfile(from, user string, names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = ShellQuote(n)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "FROM %s\nUSER root\n", from)
	b.WriteString(`RUN export LC_ALL=C DEBIAN_FRONTEND=noninteractive \
 && apt-get update -qq \
 && pkgs="" \
 && for p in ` + strings.Join(quoted, " ") + `; do if apt-cache policy "$p" 2>/dev/null | grep -q '^ *Candidate: [0-9]'; then pkgs="$pkgs $p"; fi; done \
 && if [ -n "$pkgs" ]; then apt-get install -y -qq $pkgs; fi \
 && rm -rf /var/lib/apt/lists/*
`)
	if user != "" && user != "root" {
		fmt.Fprintf(&b, "USER %s\n", user)
	}
	return b.String()
}

func dockerInspect(ctx context.Context, image, format string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", format, image).Output()
	return strings.TrimSpace(string(out)), err
}

// imagePHPModules lists the extensions php<ver> of image loads, without the
// entrypoint (which switches PHP and starts the services).
func imagePHPModules(ctx context.Context, image, ver string) ([]string, string, error) {
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--entrypoint", "php"+ver, image, "-m").CombinedOutput()
	if err != nil {
		return nil, string(out), err
	}
	return parsePHPModules(string(out)), "", nil
}
