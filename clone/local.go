package clone

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const (
	localDB       = "shopware"
	localMySQL    = "MYSQL_PWD=root mysql -h127.0.0.1 -uroot --default-character-set=utf8mb4"
	containerRoot = "/var/www/html"
)

// Local describes the generated dockware environment.
type Local struct {
	Name       string
	Dir        string // output directory with compose file, dump, logs
	CodeDir    string // project code mounted to /var/www/html
	OwnsCode   bool   // code was synced by the clone (safe to write .env)
	Port       int
	Image      string
	PHPVersion string
	AppEnv     string
	Env        map[string]string
	Redis      bool
	Frontend   *FrontendConfig
	FrontPort  int
	// MediaURL is where media and thumbnails missing locally are redirected to.
	MediaURL string
	// NodeVersion is the node major the shop container runs (via nvm); "" keeps the image default.
	NodeVersion string
	// NodePinned marks NodeVersion as requested via --node or the config, so it
	// also decides the frontend image instead of what the checkout pins.
	NodePinned bool
	// Bind is the host address the shop and frontend ports are published on; "" means 127.0.0.1.
	Bind string
	// SSHAgent mounts the host's ssh-agent into the shop container, for
	// composer packages from private VCS repositories (see NeedsSSHAgent).
	SSHAgent bool
	// Labels are set on every service; named volumes get only dockware.clone.source.
	Labels map[string]string
	// Warn receives non-fatal findings of Up; nil drops them.
	Warn func(msg string)
	// phpExts is the extension list EnsurePHPExtensions was asked for; a
	// recreated container (DropSSHAgent) needs it installed again.
	phpExts []string
	// phpNoPackage are the packages PrepareImage found without an apt
	// candidate; EnsurePHPExtensions does not look them up again.
	phpNoPackage map[string]bool
}

func (l *Local) warn(msg string) {
	if l.Warn != nil {
		l.Warn(msg)
	}
}

func (l *Local) container() string { return l.Name + "-shop" }

// ShopURL is the local storefront base URL.
func (l *Local) ShopURL() string { return "http://localhost:" + strconv.Itoa(l.Port) }

// bindAddr is the host address ports are published on (default 127.0.0.1,
// so the clone is not reachable from the network).
func (l *Local) bindAddr() string {
	if l.Bind == "" {
		return "127.0.0.1"
	}
	return l.Bind
}

// publish is a compose port mapping of host port on bindAddr.
func (l *Local) publish(host, container int) string {
	bind := l.bindAddr()
	if strings.Contains(bind, ":") && !strings.HasPrefix(bind, "[") {
		bind = "[" + bind + "]"
	}
	return fmt.Sprintf("%s:%d:%d", bind, host, container)
}

// FreePort returns the first free TCP port starting at from. A port is free
// when it can be bound on all interfaces and on bind, where Docker publishes
// it: on macOS/BSD a wildcard bind succeeds while bind:port is held. Ports in
// own count as free: a rerun recreates the project's containers, so the ports
// they publish now are released before the new ones bind.
func FreePort(from int, bind string, own map[int]bool) int {
	bind = strings.TrimSuffix(strings.TrimPrefix(bind, "["), "]")
	for p := from; p < from+200; p++ {
		if own[p] {
			return p
		}
		if canListen(":"+strconv.Itoa(p)) && (bind == "" || bind == "0.0.0.0" || bind == "::" || canListen(net.JoinHostPort(bind, strconv.Itoa(p)))) {
			return p
		}
	}
	return from
}

func canListen(addr string) bool {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// ProjectPorts lists the host ports published by the running containers of
// compose project name, e.g. the stack of a previous run. Errors (no docker,
// no containers) yield an empty set; FreePort then probes as usual.
func ProjectPorts(ctx context.Context, name string) map[int]bool {
	out, err := exec.CommandContext(ctx, "docker", "ps", "--filter", "label=com.docker.compose.project="+name, "--format", "{{.Ports}}").Output()
	if err != nil {
		return nil
	}
	return publishedPorts(string(out))
}

// publishedPorts parses docker's port column ("0.0.0.0:8320->80/tcp,
// [::]:8320->80/tcp, 6379/tcp"), one container per line, into host ports.
func publishedPorts(s string) map[int]bool {
	ports := map[int]bool{}
	for _, m := range publishedPortRE.FindAllStringSubmatch(s, -1) {
		if p, err := strconv.Atoi(m[1]); err == nil {
			ports[p] = true
		}
	}
	return ports
}

var publishedPortRE = regexp.MustCompile(`:(\d+)->`)

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// containerEnvKeys are defined by the dockware image (its ENV) or the shell.
// The entrypoint acts on PHP_VERSION and NODE_VERSION at start and exits
// when that version is not installed; an empty value counts as a version
// too, so a server .env carrying such keys must not reach shop.env at all.
// The clone sets PHP_VERSION in the compose file and switches node itself
// (UseNode).
var containerEnvKeys = map[string]bool{
	"PATH": true, "TZ": true, "PHP_VERSION": true, "NODE_VERSION": true, "NVM_VERSION": true,
	"APACHE_DOCROOT": true, "SHOP_DOMAIN": true, "RECOVERY_MODE": true, "SSH_USER": true, "SSH_PWD": true,
	"XDEBUG_REMOTE_HOST": true, "XDEBUG_CONFIG": true, "PHP_IDE_CONFIG": true, "XDEBUG_ENABLED": true,
	"FILEBEAT_ENABLED": true, "SUPERVISOR_ENABLED": true, "TIDEWAYS_KEY": true,
	"SW_CURRENCY": true, "SW_API_ACCESS_KEY": true, "SW_TASKS_ENABLED": true,
	"BASH_ENV": true, "DEBIAN_FRONTEND": true,
	"HOME": true, "HOSTNAME": true, "USER": true, "SHELL": true, "PWD": true,
}

// BuildEnv derives the local environment from the remote .env key list.
// Known keys get local values, provably harmless values are copied, every
// other remote key is set empty and reported, so no remote secret is reused.
// Keys the image owns (containerEnvKeys) are left out, also when an override
// names them; those overrides are returned as reserved so the caller can warn.
func BuildEnv(p *Probe, l *Local, overrides map[string]string) (env map[string]string, unset, reserved []string) {
	// a copied APP_DEBUG=1 doubled the local TTFB: the debug container checks
	// every file on the macOS bind mount (home 2.05 → 1.26 s, category 3.7 → 2.5 s)
	debug := "0"
	if l.AppEnv == "dev" {
		debug = "1"
	}
	env = map[string]string{
		"APP_ENV":                      l.AppEnv,
		"APP_DEBUG":                    debug,
		"APP_SECRET":                   randomHex(32),
		"APP_URL":                      l.ShopURL(),
		"APP_URL_CHECK_DISABLED":       "1",
		"DATABASE_URL":                 "mysql://root:root@127.0.0.1:3306/" + localDB,
		"MAILER_DSN":                   "smtp://127.0.0.1:1025",
		"INSTANCE_ID":                  randomHex(16),
		"LOCK_DSN":                     "flock",
		"SHOPWARE_HTTP_CACHE_ENABLED":  "0",
		"SHOPWARE_ES_ENABLED":          "0",
		"SHOPWARE_ES_INDEXING_ENABLED": "0",
		// no search node locally; a copied admin ES flag makes every indexer run log "No alive nodes found in cluster"
		"SHOPWARE_ADMIN_ES_ENABLED":            "0",
		"SHOPWARE_ADMIN_ES_REFRESH_INDICES":    "0",
		"OPENSEARCH_URL":                       "http://localhost:9200",
		"MESSENGER_TRANSPORT_DSN":              "doctrine://default?auto_setup=1",
		"MESSENGER_TRANSPORT_LOW_PRIORITY_DSN": "doctrine://default?auto_setup=1&queue_name=low_priority",
		"MESSENGER_TRANSPORT_FAILURE_DSN":      "doctrine://default?queue_name=failed&auto_setup=1",
		"BLUE_GREEN_DEPLOYMENT":                "0",
		"COMPOSER_HOME":                        "/var/www/.composer",
		// install.lock is not copied (PostImport creates it); this covers index.php versions that read the env instead
		"SHOPWARE_SKIP_WEBINSTALLER": "1",
	}
	known := map[string]bool{}
	for k := range env {
		known[k] = true
	}
	for _, k := range p.EnvKeys {
		if known[k] || containerEnvKeys[k] {
			continue
		}
		switch {
		case strings.HasPrefix(k, "REDIS") && strings.HasSuffix(k, "HOST"):
			env[k] = "redis"
		case strings.HasPrefix(k, "REDIS") && strings.HasSuffix(k, "PORT"):
			env[k] = "6379"
		case strings.HasPrefix(k, "REDIS") && strings.Contains(k, "PASS"):
			env[k] = ""
		case strings.HasPrefix(k, "REDIS") && (strings.Contains(k, "URL") || strings.Contains(k, "DSN")):
			// REDIS_URL, REDIS_DSN_CACHE, REDIS_DSN_CACHE_HTTP, REDIS_URL_SESSION
			env[k] = "redis://redis:6379"
		case redisSchemes[p.EnvSchemes[k]]:
			// CART_DSN, SESSION_DSN, …: secret-looking keys whose server value is a redis URL
			env[k] = "redis://redis:6379"
		default:
			if v, ok := p.SafeEnv[k]; ok {
				env[k] = v
			} else {
				env[k] = ""
				unset = append(unset, k)
			}
		}
	}
	for k, v := range overrides {
		if containerEnvKeys[k] {
			reserved = append(reserved, k)
			continue
		}
		env[k] = v
		unset = remove(unset, k)
	}
	sort.Strings(unset)
	sort.Strings(reserved)
	return env, unset, reserved
}

func remove(list []string, v string) []string {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// redisSchemes are the URL schemes Shopware's RedisConnectionFactory accepts.
var redisSchemes = map[string]bool{"redis": true, "rediss": true, "valkey": true, "valkeys": true}

// NeedsRedis reports whether the remote config uses Redis: a REDIS* key or a
// DSN/URL key whose server value has a redis scheme.
func NeedsRedis(p *Probe) bool {
	for _, k := range p.EnvKeys {
		if strings.HasPrefix(k, "REDIS") {
			return true
		}
	}
	for _, s := range p.EnvSchemes {
		if redisSchemes[s] {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func dotenv(env map[string]string) string {
	var b strings.Builder
	b.WriteString("# generated by dockware-cli clone, local values only\n")
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(&b, "%s=%s\n", k, strconv.Quote(env[k]))
	}
	return b.String()
}

// apacheEnv passes the environment to PHP-FPM (which clears the process env).
func apacheEnv(env map[string]string) string {
	var b strings.Builder
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(&b, "SetEnv %s %s\n", k, strconv.Quote(env[k]))
	}
	return b.String()
}

func dockerEnvFile(env map[string]string) string {
	var b strings.Builder
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(&b, "%s=%s\n", k, env[k])
	}
	return b.String()
}

// DockerAvailable fails when the docker CLI is missing or no daemon answers.
func DockerAvailable(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput()
	if err == nil {
		return nil
	}
	linux := runtime.GOOS == "linux"
	if errors.Is(err, exec.ErrNotFound) {
		hint := "install Docker Desktop and retry"
		if linux {
			hint = "install Docker Engine with the Compose v2 plugin and retry"
		}
		return &Error{Step: StepDocker, Msg: "docker is not installed (no docker binary in PATH)", Hint: hint, Err: err}
	}
	failed := Fail(StepDocker, "Docker is not available", err, tailLines(string(out), 10))
	var e *Error
	if linux && errors.As(failed, &e) && (e.Hint == "" || strings.Contains(e.Hint, "Docker Desktop")) {
		e.Hint = "Docker is not running or not accessible: start Docker (sudo systemctl start docker) and make sure your user may use it (docker group), then retry"
	}
	return failed
}

// LocalPreflight checks the local tools the clone needs before anything is
// copied from the server: Docker Compose v2 and, when code is synced, rsync.
func LocalPreflight(ctx context.Context, needRsync bool) error {
	if out, err := exec.CommandContext(ctx, "docker", "compose", "version").CombinedOutput(); err != nil {
		return &Error{
			Step:   StepDocker,
			Msg:    "`docker compose version` failed",
			Hint:   "install Docker Compose v2 (the `docker compose` plugin; the standalone docker-compose v1 is not supported) and retry",
			Output: tailLines(string(out), 10),
			Err:    err,
		}
	}
	if needRsync {
		if _, err := exec.LookPath("rsync"); err != nil {
			return &Error{
				Step: StepRsync,
				Msg:  "rsync is not installed (no rsync binary in PATH)",
				Hint: "install rsync locally (it syncs the code from the server), or pass --code with an existing checkout",
				Err:  err,
			}
		}
	}
	return nil
}

// EngineMismatch returns a warning when the server database (its version
// string) differs from the local MySQL 8 engine, "" when they are compatible.
func EngineMismatch(mysqlVersion string) string {
	if !strings.Contains(strings.ToLower(mysqlVersion), "mariadb") {
		return ""
	}
	return "the server runs MariaDB (" + strings.TrimSpace(mysqlVersion) + ") but the local stack runs MySQL 8: " +
		"the dump may fail to import or behave differently (MariaDB-only collations such as utf8mb4_uca1400_ai_ci, " +
		"sequences, JSON stored as LONGTEXT); --strategy shadow does not help, it dumps from the same engine. Verify the clone yourself"
}
