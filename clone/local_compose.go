package clone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// phpCloneINI raises the image's OPcache limits for a shop checkout: its
// 10000 accelerated files are below the ~23k vendor files plus the
// container cache, revalidate_freq 2 keeps code edits visible within 2 s,
// and the realpath cache spares stat calls on the bind mount.
const phpCloneINI = `opcache.memory_consumption=512
opcache.interned_strings_buffer=64
opcache.max_accelerated_files=100000
opcache.revalidate_freq=2
realpath_cache_size=4096K
realpath_cache_ttl=600
`

// WriteFiles writes compose file, env files, Apache env include and php ini.
func (l *Local) WriteFiles() error {
	if err := writeFile(filepath.Join(l.Dir, "shop.env"), []byte(dockerEnvFile(l.Env)), 0o600); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(l.Dir, "apache-env.conf"), []byte(apacheEnv(l.Env)), 0o644); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(l.Dir, "php-clone.ini"), []byte(phpCloneINI), 0o644); err != nil {
		return err
	}
	if l.MediaURL != "" {
		if err := writeFile(filepath.Join(l.Dir, "apache-media.conf"), []byte(mediaFallback(l.MediaURL)), 0o644); err != nil {
			return err
		}
	}
	if l.Frontend != nil {
		if err := writeFile(filepath.Join(l.Dir, "apache-cors.conf"), []byte(frontendCORS), 0o644); err != nil {
			return err
		}
	}
	if l.OwnsCode {
		if err := writeFile(filepath.Join(l.CodeDir, ".env"), []byte(dotenv(l.Env)), 0o600); err != nil {
			return err
		}
	}
	compose, err := l.Compose()
	if err != nil {
		return Fail(StepConfig, "rendering docker-compose.yml failed", err, "")
	}
	if l.Frontend != nil {
		// compose validates env_file of inactive profiles too
		if _, err := os.Stat(filepath.Join(l.Dir, "frontend.env")); os.IsNotExist(err) {
			if err := writeFile(filepath.Join(l.Dir, "frontend.env"), nil, 0o600); err != nil {
				return err
			}
		}
	}
	return writeFile(filepath.Join(l.Dir, "docker-compose.yml"), compose, 0o644)
}

// writeFile classifies a failed write of a generated file as a config error.
func writeFile(path string, data []byte, perm os.FileMode) error {
	if err := os.WriteFile(path, data, perm); err != nil {
		return Fail(StepConfig, "writing "+path+" failed", err, "")
	}
	return nil
}

func (l *Local) shopVolumes() []string {
	vol := func(name, path string) string { return name + ":" + containerRoot + path }
	v := []string{
		l.CodeDir + ":" + containerRoot,
		"./apache-env.conf:/etc/apache2/conf-enabled/zz-dockware-clone-env.conf:ro",
	}
	if l.MediaURL != "" {
		v = append(v, "./apache-media.conf:/etc/apache2/conf-enabled/zz-dockware-clone-media.conf:ro")
	}
	if l.Frontend != nil {
		v = append(v, "./apache-cors.conf:/etc/apache2/conf-enabled/zz-dockware-clone-cors.conf:ro")
	}
	if l.PHPVersion != "" {
		// the ini dir is per PHP version, so without a known version the image defaults stay
		for _, sapi := range []string{"fpm", "cli"} {
			v = append(v, "./php-clone.ini:/etc/php/"+phpMajorMinor(l.PHPVersion)+"/"+sapi+"/conf.d/99-dockware-clone.ini:ro")
		}
	}
	v = append(v, vol("vendor", "/vendor"))
	if !l.OwnsCode {
		// a developer checkout: nothing Shopware writes may land in git (see SeedVar)
		v = append(v, vol("var", "/var"))
	}
	return append(v,
		vol("cache", "/var/cache"),
		vol("log", "/var/log"),
		vol("theme", "/public/theme"),
		vol("bundles", "/public/bundles"),
		"db:/var/lib/mysql",
	)
}

var mediaURLRe = regexp.MustCompile(`^https?://[^\s"'$%\\]+$`)

// ValidMediaURL reports whether url is safe to embed in the Apache rewrite.
func ValidMediaURL(url string) bool { return mediaURLRe.MatchString(url) }

// frontendCORS answers preflights with the headers asked for: the frontend
// runs on another origin than the shop, and Shopware allows only its own
// list, without e.g. sw-currency-id.
const frontendCORS = `Header always set Access-Control-Allow-Headers "expr=%{req:Access-Control-Request-Headers}" "expr=-n %{req:Access-Control-Request-Headers}"
`

// mediaFallback redirects media and thumbnail requests whose file does not
// exist locally to the source, so the clone needs no media copy while local
// uploads and an existing public/media in a checkout still win. Shopware's
// .htaccess enables its own rewrite engine, hence InheritDownBefore.
func mediaFallback(url string) string {
	return `<Directory "` + containerRoot + `/public">
    RewriteEngine On
    RewriteOptions InheritDownBefore
    RewriteCond %{REQUEST_FILENAME} !-f
    RewriteRule ^((?:media|thumbnail)/.+)$ ` + strings.TrimRight(url, "/") + `/$1 [R=302,L,NE]
</Directory>
`
}

// volumeLabel is the only label of Local.Labels copied onto named volumes.
const volumeLabel = "dockware.clone.source"

// Compose renders docker-compose.yml with this volume layout:
// code bind-mounted, vendor/cache/log/theme/bundles/node_modules in volumes.
func (l *Local) Compose() ([]byte, error) {
	var environment []string
	if l.PHPVersion != "" {
		environment = append(environment, "PHP_VERSION="+phpMajorMinor(l.PHPVersion))
	}
	// NODE_VERSION stays unset: the entrypoint relinks node to it without
	// installing it, which breaks node when the image lacks that version (see UseNode)
	environment = append(environment, "XDEBUG_ENABLED=0")
	volumes := l.shopVolumes()
	if sock := hostSSHAgent(); l.SSHAgent && sock != "" {
		// private VCS packages (git@github.com:…) need the developer's keys;
		// only while composer runs (DropSSHAgent), never for a plain clone
		volumes = append(volumes, sock+":"+agentSocket)
		environment = append(environment, "SSH_AUTH_SOCK="+agentSocket)
	}
	shop := map[string]any{
		"image":          l.Image,
		"container_name": l.container(),
		"hostname":       "shop",
		"ports":          []string{l.publish(l.Port, 80)},
		"env_file":       []string{"shop.env"},
		"environment":    environment,
		"volumes":        volumes,
	}
	services := map[string]any{"shop": shop}
	volumeNames := []string{"vendor", "cache", "log", "theme", "bundles", "db"}
	if !l.OwnsCode {
		volumeNames = append(volumeNames, "var")
	}
	if l.Redis {
		services["redis"] = map[string]any{"image": "redis:7-alpine", "container_name": l.Name + "-redis"}
		shop["depends_on"] = []string{"redis"}
	}
	if f := l.Frontend; f != nil {
		app := "/app"
		cmd := f.Command
		if f.Install != "" {
			cmd = f.Install + " && " + f.Command
		}
		cmd = l.frontendForwarder(f.Port) + cmd
		services["frontend"] = map[string]any{
			"image":          l.FrontendImage(),
			"container_name": l.Name + "-frontend",
			"working_dir":    app,
			"ports":          []string{l.publish(l.FrontPort, f.Port)},
			"env_file":       []string{"frontend.env"},
			"command":        []string{"sh", "-c", cmd},
			"depends_on":     []string{"shop"},
			"profiles":       []string{"frontend"},
			// .output stays on the bind mount: `nuxt dev` removes that directory,
			// which fails with EBUSY on a volume mountpoint
			"volumes": []string{
				filepath.Join(l.CodeDir, f.Path) + ":" + app,
				"node_modules:" + app + "/node_modules",
				"nuxt:" + app + "/.nuxt",
			},
		}
		volumeNames = append(volumeNames, "node_modules", "nuxt")
	}
	if len(l.Labels) > 0 {
		for _, s := range services {
			s.(map[string]any)["labels"] = l.Labels
		}
	}
	// volumes get only the stable source label: created/expires change on every
	// rerun into the same out dir, and compose treats a volume whose labels
	// differ from the file as diverged config. A volume without settings
	// renders as null, as compose expects.
	var volumeDef any
	if src, ok := l.Labels[volumeLabel]; ok {
		volumeDef = map[string]any{"labels": map[string]string{volumeLabel: src}}
	}
	volumeDefs := map[string]any{}
	for _, v := range volumeNames {
		volumeDefs[v] = volumeDef
	}
	return yaml.Marshal(map[string]any{"name": l.Name, "services": services, "volumes": volumeDefs})
}

// frontendForwarder starts a TCP forwarder in the frontend container, so the
// browser URLs (${SHOP_URL}, ${FRONTEND_URL} on localhost) that a frontend
// also calls while rendering on the server reach the shop and the dev server
// there; without it they hit the container itself. Needs node in the image.
func (l *Local) frontendForwarder(appPort int) string {
	var routes []string
	if l.Port != appPort {
		routes = append(routes, fmt.Sprintf(`[%d,"shop",80]`, l.Port))
	}
	if l.FrontPort != appPort && l.FrontPort != l.Port {
		routes = append(routes, fmt.Sprintf(`[%d,"127.0.0.1",%d]`, l.FrontPort, appPort))
	}
	if len(routes) == 0 {
		return ""
	}
	js := `const n=require("net");[` + strings.Join(routes, ",") + `].forEach(([p,h,t])=>n.createServer(c=>{const s=n.connect(t,h);c.pipe(s).pipe(c);s.on("error",()=>c.destroy());c.on("error",()=>s.destroy())}).listen(p,"127.0.0.1"))`
	return `if command -v node >/dev/null; then node -e '` + js + `' & else echo "no node in the image: localhost:` + strconv.Itoa(l.Port) + ` does not reach the shop while rendering on the server"; fi; `
}

// compose runs docker compose; its progress output is only shown on failure,
// classified under step (the shop or the frontend service).
func (l *Local) compose(ctx context.Context, step string, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-f", filepath.Join(l.Dir, "docker-compose.yml")}, args...)...)
	cmd.Dir = l.Dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return Fail(step, "docker compose "+strings.Join(args, " ")+" failed", err, tailLines(string(out), 20))
	}
	return nil
}

// Exec runs a shell script in the shop container as www-data.
func (l *Local) Exec(ctx context.Context, script string, stdin io.Reader, stdout, stderr io.Writer) error {
	args := []string{"exec", "-i", "-w", containerRoot, l.container(), "bash", "-c", script}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

// execRoot runs a shell script in the shop container as root and returns
// its combined output; callers classify the failure.
func (l *Local) execRoot(ctx context.Context, script string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "exec", "-u", "root", l.container(), "bash", "-c", script).CombinedOutput()
	return string(out), err
}

// Console runs bin/console and returns combined output.
func (l *Local) Console(ctx context.Context, args ...string) (string, error) {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = ShellQuote(a)
	}
	var out bytes.Buffer
	err := l.Exec(ctx, "php -d memory_limit=-1 bin/console "+strings.Join(quoted, " "), nil, &out, &out)
	if err != nil {
		return out.String(), fmt.Errorf("bin/console %s: %w", strings.Join(args, " "), err)
	}
	return out.String(), nil
}

// SQL runs SQL in the local database and returns tab separated rows.
func (l *Local) SQL(ctx context.Context, sql string) (string, error) {
	var out, errb bytes.Buffer
	if err := l.Exec(ctx, localMySQL+" -N -B "+localDB, strings.NewReader(sql), &out, &errb); err != nil {
		return "", fmt.Errorf("local mysql: %w: %s", err, errb.String())
	}
	return out.String(), nil
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Up starts the shop container, waits for MySQL and Apache and checks that
// the image runs the PHP version the shop needs. A container that exits
// while starting (entrypoint failure) is reported at once with its logs.
func (l *Local) Up(ctx context.Context) error {
	if err := l.compose(ctx, StepDocker, "up", "-d", "shop"); err != nil {
		return err
	}
	return l.started(ctx)
}

// started waits for a (re)created shop container and prepares it.
func (l *Local) started(ctx context.Context) error {
	if err := l.waitReady(ctx); err != nil {
		return err
	}
	// fresh named volumes are root-owned; the image ships one ssh key pair
	// identical in every pull, which must never authenticate anything
	out, err := l.execRoot(ctx, "chown www-data:www-data "+strings.Join([]string{
		containerRoot + "/vendor", containerRoot + "/var/cache", containerRoot + "/var/log",
		containerRoot + "/public/theme", containerRoot + "/public/bundles",
	}, " ")+" && rm -f /var/www/.ssh/id_rsa /var/www/.ssh/id_rsa.pub")
	if err != nil {
		return Fail(StepDocker, "preparing the volumes of "+l.container()+" failed", err, tailLines(out, 20))
	}
	l.prepareCodeMount(ctx)
	return l.checkPHP(ctx)
}

// waitReady waits until the dockware entrypoint reports the container ready.
func (l *Local) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(5 * time.Minute)
	var logs []byte
	for time.Now().Before(deadline) {
		logs, _ = exec.CommandContext(ctx, "docker", "logs", "--tail", "40", l.container()).CombinedOutput()
		if bytes.Contains(logs, []byte("container IS READY")) {
			return nil
		}
		if status, code := l.containerState(ctx); status == "exited" || status == "dead" {
			e := &Error{
				Step:   StepDocker,
				Msg:    fmt.Sprintf("container %s exited with code %d during start", l.container(), code),
				Output: tailLines(string(logs), 20),
			}
			if e.Hint = hintFor(StepDocker, string(logs)); e.Hint == "" {
				e.Hint = "the entrypoint of " + l.Image + " failed: see `docker logs " + l.container() + "`"
			}
			return e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return &Error{
		Step:   StepDocker,
		Msg:    "container " + l.container() + " did not become ready within 5 minutes",
		Hint:   "the container is still starting: check `docker logs " + l.container() + "`; the first start of an image is slow (nvm decompression, PHP switch), otherwise a wrong --image / --php is the usual cause",
		Output: tailLines(string(logs), 20),
	}
}

// codeWritePaths are the directories of the code bind mount Shopware writes to.
var codeWritePaths = []string{"files", "public", "config/jwt", "custom/plugins"}

// prepareCodeMount makes the directories Shopware writes on the code bind
// mount writable for www-data (uid 33) on Linux, where a bind mount keeps
// the host owner; Docker Desktop maps it. The checkout is never chowned:
// code the clone synced gets a+rwX on these directories (and var, which is
// not a volume then), a developer checkout an ACL for uid 33 when the image
// has setfacl, else a warning with the host command. Missing directories
// are created as the user, so the checkout keeps its owner.
func (l *Local) prepareCodeMount(ctx context.Context) {
	if runtime.GOOS != "linux" || l.CodeDir == "" {
		return
	}
	paths := codeWritePaths
	if l.OwnsCode {
		paths = append(append([]string{}, paths...), "var")
	}
	var rel, host []string
	for _, p := range paths {
		dir := filepath.Join(l.CodeDir, filepath.FromSlash(p))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			l.warn("cannot create " + dir + ": " + err.Error())
			continue
		}
		rel = append(rel, ShellQuote(p))
		host = append(host, ShellQuote(dir))
	}
	if len(rel) == 0 {
		return
	}
	cd := "cd " + containerRoot + " && "
	if l.OwnsCode {
		if out, err := l.execRoot(ctx, cd+"chmod -R a+rwX "+strings.Join(rel, " ")); err != nil {
			l.warn("making " + strings.Join(host, " ") + " writable for the web server failed (" + tailLines(out, 1) + "); run on the host: sudo chmod -R a+rwX " + strings.Join(host, " "))
		}
		return
	}
	out, err := l.execRoot(ctx, "command -v setfacl >/dev/null 2>&1 || exit 3\n"+cd+"setfacl -R -m u:33:rwX "+strings.Join(rel, " "))
	if err == nil {
		return
	}
	reason := "the image has no setfacl"
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		reason = "setfacl failed: " + tailLines(out, 1)
	}
	l.warn("the web server (www-data, uid 33) may not write to " + strings.Join(host, " ") + " in your checkout (" + reason + "); run on the host: sudo setfacl -R -m u:33:rwX " + strings.Join(host, " "))
}

// Down removes the project's containers and network, with volumes also its
// named volumes (the cloned database). Without a compose file there is
// nothing of this project to remove.
func (l *Local) Down(ctx context.Context, volumes bool) error {
	file := filepath.Join(l.Dir, "docker-compose.yml")
	if _, err := os.Stat(file); os.IsNotExist(err) {
		return nil
	}
	// the frontend profile is named so its container goes too
	args := []string{"compose", "-p", l.Name, "-f", file, "--profile", "frontend", "down"}
	if volumes {
		args = append(args, "-v")
	}
	args = append(args, "--remove-orphans")
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = l.Dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return Fail(StepDocker, "docker "+strings.Join(args, " ")+" failed", err, tailLines(string(out), 20))
	}
	return nil
}

// containerState returns status and exit code of the shop container from
// docker inspect; a container that cannot be inspected reports "" and 0.
func (l *Local) containerState(ctx context.Context) (status string, exitCode int) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Status}} {{.State.ExitCode}}", l.container()).Output()
	if err != nil {
		return "", 0
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return "", 0
	}
	exitCode, _ = strconv.Atoi(fields[1])
	return fields[0], exitCode
}
