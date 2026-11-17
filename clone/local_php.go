package clone

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
)

// checkPHP fails when the image does not run the PHP version the shop needs.
func (l *Local) checkPHP(ctx context.Context) error {
	if l.PHPVersion == "" {
		return nil
	}
	want := phpMajorMinor(l.PHPVersion)
	got, err := l.phpVersion(ctx)
	if err != nil {
		return err
	}
	if got != want {
		return &Error{
			Step: StepDocker,
			Msg:  fmt.Sprintf("image %s does not provide PHP %s (got %s)", l.Image, want, got),
			Hint: "pass --image with an image that has PHP " + want + " or override with --php / php: in .dockware-clone.yml",
		}
	}
	return nil
}

// phpVersion reads major.minor of the container's php.
func (l *Local) phpVersion(ctx context.Context) (string, error) {
	var out, errb bytes.Buffer
	if err := l.Exec(ctx, `php -r 'echo PHP_MAJOR_VERSION.".".PHP_MINOR_VERSION;'`, nil, &out, &errb); err != nil {
		return "", Fail(StepDocker, "reading the PHP version of "+l.container()+" failed", err, tailLines(errb.String(), 20))
	}
	return strings.TrimSpace(out.String()), nil
}

// phpMajorMinor reduces "8.3.12" to "8.3", the granularity dockware switches.
func phpMajorMinor(v string) string {
	v = strings.TrimSpace(v)
	if parts := strings.SplitN(v, ".", 3); len(parts) > 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}

// phpExtIgnored are profilers and debuggers: no code path requires them, so
// a clone does not install them. OPcache lists itself as a PHP module too.
var phpExtIgnored = map[string]bool{
	"tideways": true, "xdebug": true, "pcov": true, "blackfire": true, "newrelic": true,
	"opcache": true, "zend opcache": true, "ddtrace": true, "excimer": true,
}

// phpExtPackageAlias names the package part of extensions that do not come
// as php<ver>-<ext>: the pdo drivers ship with their database package.
var phpExtPackageAlias = map[string]string{
	"pdo_sqlite": "sqlite3", "pdo_pgsql": "pgsql", "pdo_mysql": "mysql", "mysqli": "mysql", "mysqlnd": "mysql",
}

// phpPackage is one apt package and the missing extensions it provides.
type phpPackage struct {
	Name string
	Exts []string
}

// parsePHPModules returns the modules of a `php -m` output lowercase and
// sorted: the [PHP Modules] section only, so Zend extensions listed under
// [Zend Modules] are left out. Output without the section yields nil.
func parsePHPModules(out string) []string {
	var mods []string
	in := false
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "[PHP Modules]":
			in = true
		case strings.HasPrefix(l, "["):
			in = false
		case in && l != "":
			mods = append(mods, strings.ToLower(l))
		}
	}
	sort.Strings(mods)
	return mods
}

// phpExtPackages maps the extensions of want that have lacks to the apt
// packages providing them, php<ver>-<ext> with the pdo aliases, sorted by
// package name; one package may cover several extensions (pdo_sqlite and
// sqlite3 both come with php8.3-sqlite3). Profilers and debuggers are left out.
func phpExtPackages(ver string, want, have []string) []phpPackage {
	has := make(map[string]bool, len(have))
	for _, e := range have {
		has[e] = true
	}
	byPkg := map[string][]string{}
	for _, e := range want {
		if has[e] || phpExtIgnored[e] {
			continue
		}
		has[e] = true // once per extension
		name := e
		if alias, ok := phpExtPackageAlias[e]; ok {
			name = alias
		}
		pkg := "php" + ver + "-" + name
		byPkg[pkg] = append(byPkg[pkg], e)
	}
	pkgs := make([]phpPackage, 0, len(byPkg))
	for name, exts := range byPkg {
		sort.Strings(exts)
		pkgs = append(pkgs, phpPackage{Name: name, Exts: exts})
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].Name < pkgs[j].Name })
	return pkgs
}

// phpModules lists the extensions of the container's php.
func (l *Local) phpModules(ctx context.Context) ([]string, error) {
	var out, errb bytes.Buffer
	if err := l.Exec(ctx, "php -m", nil, &out, &errb); err != nil {
		return nil, Fail(StepDocker, "listing the PHP extensions of "+l.container()+" failed", err, tailLines(errb.String(), 20))
	}
	return parsePHPModules(out.String()), nil
}

// EnsurePHPExtensions installs the extensions of want (the server's php -m)
// that the container's php lacks from the image's apt sources
// (php<ver>-<ext>) and restarts php-fpm, so composer's platform check and
// code paths needing them (zstd cache compression, imagick, ...) work. It
// returns the extensions still missing afterwards: those without a package,
// or all of them when apt failed; the clone goes on and the caller warns. A
// broken docker exec is the only error. The packages live in the container
// filesystem and are gone once compose recreates the container; want is
// kept, so DropSSHAgent installs them again.
func (l *Local) EnsurePHPExtensions(ctx context.Context, want []string, log io.Writer) ([]string, error) {
	l.phpExts = want
	if len(want) == 0 {
		return nil, nil
	}
	have, err := l.phpModules(ctx)
	if err != nil {
		return nil, err
	}
	ver := phpMajorMinor(l.PHPVersion)
	if ver == "" {
		if ver, err = l.phpVersion(ctx); err != nil {
			return nil, err
		}
	}
	pkgs := phpExtPackages(ver, want, have)
	if len(pkgs) == 0 {
		return nil, nil
	}
	names := make([]string, len(pkgs))
	var exts []string
	for i, p := range pkgs {
		names[i] = ShellQuote(p.Name)
		exts = append(exts, p.Exts...)
	}
	sort.Strings(exts)
	// the image ships without package lists; only packages with a candidate
	// are installed, so an extension without one does not fail the others
	out, err := l.execRoot(ctx, `export LC_ALL=C DEBIAN_FRONTEND=noninteractive
out=$(apt-get update -qq 2>&1) || { printf '%s\n' "$out"; exit 1; }
for p in `+strings.Join(names, " ")+`; do apt-cache policy "$p" 2>/dev/null | grep -q '^ *Candidate: [0-9]' && echo "$p"; done
true`)
	if err != nil {
		fmt.Fprintf(log, "  php extensions: apt-get update failed, %s stay missing:\n    %s\n", strings.Join(exts, ", "), strings.ReplaceAll(tailLines(out, 10), "\n", "\n    "))
		return exts, nil
	}
	candidate := map[string]bool{}
	for _, p := range strings.Fields(out) {
		candidate[p] = true
	}
	var install, noPackage []string
	for _, p := range pkgs {
		if candidate[p.Name] {
			install = append(install, p.Name)
		} else {
			noPackage = append(noPackage, p.Name)
		}
	}
	if len(noPackage) > 0 {
		fmt.Fprintf(log, "  php extensions: no apt package %s\n", strings.Join(noPackage, ", "))
	}
	if len(install) == 0 {
		return exts, nil
	}
	if out, err = l.execRoot(ctx, "export DEBIAN_FRONTEND=noninteractive\napt-get install -y -qq "+strings.Join(install, " ")+" 2>&1 || exit 1\nservice php"+ver+"-fpm restart >/dev/null 2>&1 || true"); err != nil {
		fmt.Fprintf(log, "  php extensions: apt-get install %s failed:\n    %s\n", strings.Join(install, " "), strings.ReplaceAll(tailLines(out, 10), "\n", "\n    "))
	}
	// what php reports now decides, not apt's exit status (partial installs)
	if have, err = l.phpModules(ctx); err != nil {
		return nil, err
	}
	var missing []string
	for _, p := range phpExtPackages(ver, exts, have) {
		missing = append(missing, p.Exts...)
	}
	sort.Strings(missing)
	var installed []string
	for _, e := range exts {
		if !containsString(missing, e) {
			installed = append(installed, e)
		}
	}
	if len(installed) > 0 {
		fmt.Fprintf(log, "  php extensions: installed %s\n", strings.Join(installed, ", "))
	}
	return missing, nil
}
