package clone

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Tuning for the local MySQL: a throwaway dev database, so durability is
// traded for import and indexer speed; a 4 GiB buffer pool fits Docker
// Desktop's default memory limit, smaller VMs get a quarter of their RAM.
const (
	mysqlBufferPoolMax   int64 = 4 << 30
	mysqlBufferPoolChunk int64 = 128 << 20 // innodb resizes the pool in chunks of this size
	mysqlRedoLogBytes    int64 = 2 << 30
	mysqlFlushAtCommit         = 2
)

// bufferPoolBytes returns the innodb_buffer_pool_size for a container with
// memTotalKB of RAM: a quarter of it, at most mysqlBufferPoolMax, rounded
// down to a whole chunk.
func bufferPoolBytes(memTotalKB int64) int64 {
	pool := memTotalKB * 1024 / 4
	if pool > mysqlBufferPoolMax {
		pool = mysqlBufferPoolMax
	}
	return pool - pool%mysqlBufferPoolChunk
}

// tuneMySQL persists the tuning in the db volume: SET PERSIST writes
// mysqld-auto.cnf into the datadir, so it also covers later indexer runs and
// a recreated container. Every statement is sent on its own, so a variable
// this server lacks (innodb_redo_log_capacity before MySQL 8.0.30, MariaDB)
// skips only itself. Failures are logged, never fatal.
func (l *Local) tuneMySQL(ctx context.Context, log io.Writer) {
	stmts := []string{
		fmt.Sprintf("SET PERSIST innodb_flush_log_at_trx_commit = %d", mysqlFlushAtCommit),
		fmt.Sprintf("SET PERSIST innodb_redo_log_capacity = %d", mysqlRedoLogBytes),
	}
	var out bytes.Buffer
	skipped := func(what string) {
		fmt.Fprintf(log, "  mysql tuning skipped: %s: %s\n", what, tailLines(out.String(), 1))
		out.Reset()
	}
	// the pool is sized from the container's RAM and never shrunk
	if err := l.Exec(ctx, "awk '/^MemTotal:/{print $2}' /proc/meminfo && "+localMySQL+" -N -B -e 'SELECT @@innodb_buffer_pool_size'", nil, &out, &out); err != nil {
		skipped("innodb_buffer_pool_size")
	} else if fields := strings.Fields(out.String()); len(fields) != 2 {
		skipped("innodb_buffer_pool_size: expected MemTotal and the current size")
	} else {
		memKB, _ := strconv.ParseInt(fields[0], 10, 64)
		current, _ := strconv.ParseInt(fields[1], 10, 64)
		if pool := bufferPoolBytes(memKB); pool > current {
			stmts = append(stmts, fmt.Sprintf("SET PERSIST innodb_buffer_pool_size = %d", pool))
		}
		out.Reset()
	}
	for _, stmt := range stmts {
		if err := l.Exec(ctx, localMySQL+" -e "+ShellQuote(stmt), nil, io.Discard, &out); err != nil {
			skipped(stmt)
		}
	}
}

// Parallel import: the dump is split in the container into DDL, data parts
// and a trailer. Per-table files loaded in parallel deadlock on DROP/CREATE
// of tables with foreign keys (ERROR 1213); all DDL first, then the data,
// does not. 128 MiB parts balance 8 loaders; more loaders than CPUs
// (nproc) are never started. importDir prefixes the per-run work dir
// (mktemp) and the pid file of the running import script.
const (
	importWorkers         = 8
	importPartBytes int64 = 128 << 20
	importDir             = "/tmp/dockware-clone-import"
)

// importSplitter is the awk program (-v dir=… -v max=…) that splits a
// shopware-cli dump: everything outside data sections goes to schema.sql in
// stream order; the INSERTs of a `-- Data for table` section go to numbered
// *.part.sql files that break only between statements, close at about max
// bytes and never share a table; views and triggers (shopware-cli writes
// them after the last table) and whatever else follows a table's data (the
// FOREIGN_KEY_CHECKS = 1 trailer) go to post.sql, which runs last, so no
// trigger fires during the parallel row load. LOCK/UNLOCK TABLES and the
// MyISAM key hints are dropped, they only serialise. Every output file
// starts with the dump's own session settings. Must stay POSIX awk: the
// image may ship mawk.
const importSplitter = `
function closepart() { if (part != "") close(part); part = ""; size = 0 }
function newpart() { closepart(); n++; part = sprintf("%s/%05d.part.sql", dir, n); printf "%s", pre > part; size = length(pre) }
function topost() { if (!postopen) { printf "%s", pre > post; postopen = 1 }; mode = "post" }
BEGIN {
	pre = "SET NAMES utf8mb4;\nSET FOREIGN_KEY_CHECKS = 0;\nSET UNIQUE_CHECKS = 0;\n"
	schema = dir "/schema.sql"
	post = dir "/post.sql"
	printf "%s", pre > schema
	mode = "schema"
}
/^-- (Trigger|Structure for view) / { closepart(); topost(); print > post; next }
/^-- Structure for / { closepart(); mode = "schema"; print > schema; next }
/^-- Data for table / { closepart(); mode = "data"; stmt = 0; next }
mode == "schema" { print > schema; next }
mode == "post" { print > post; next }
stmt { print > part; size += length($0) + 1; if ($0 ~ /;[ \t\r]*$/) stmt = 0; next }
/^INSERT INTO / { if (part == "" || size >= max) newpart(); print > part; size += length($0) + 1; if ($0 !~ /;[ \t\r]*$/) stmt = 1; next }
/^(LOCK|UNLOCK) TABLES/ || /^\/\*!40000 ALTER TABLE / || /^--/ || /^[ \t\r]*$/ { next }
{ topost(); print > post }
END { closepart() }
`

// importScript builds the container-side import that reads the compressed
// dump on stdin: split it, load schema.sql serially, the parts with up to
// workers mysql clients largest first (ls -S), then post.sql. The work dir
// is per run and removed on every exit; a loaded part is deleted at once to
// bound the disk use. The script's pid is kept in the pid file so
// killImportScript can stop it after the docker exec client is gone.
// DEFINER clauses are stripped because the remote MySQL user does not exist
// locally; no import session writes a binary log.
func importScript(decompress string, workers int, partBytes int64) string {
	load := localMySQL + ` --max-allowed-packet=512M --init-command="SET SESSION sql_log_bin=0" ` + localDB
	w := strconv.Itoa(workers)
	return `set -o pipefail
dir=$(mktemp -d ` + importDir + `.XXXXXX) || exit 1
echo $$ > ` + importDir + `.pid
trap 'rm -rf "$dir" ` + importDir + `.pid' EXIT
LC_ALL=C ` + decompress + ` | LC_ALL=C sed -E 's/DEFINER=` + "`[^`]+`@`[^`]+`" + `//g' | LC_ALL=C awk -v dir="$dir" -v max=` + strconv.FormatInt(partBytes, 10) + ` ` + ShellQuote(importSplitter) + ` || exit 1
` + load + ` < "$dir/schema.sql" || exit 1
workers=$(nproc 2>/dev/null || echo 1)
if [ "$workers" -gt ` + w + ` ]; then workers=` + w + `; fi
parts=$(ls -S "$dir"/*.part.sql 2>/dev/null)
if [ -n "$parts" ]; then
	printf '%s\n' "$parts" | xargs -P "$workers" -n 1 bash -c ` + ShellQuote(load+` < "$1" && rm -f -- "$1" || { echo "loading $1 failed" >&2; exit 1; }`) + ` _ || exit 1
fi
if [ -s "$dir/post.sql" ]; then ` + load + ` < "$dir/post.sql" || exit 1; fi
`
}

// killImportScript stops an import script whose docker exec client died
// (Ctrl+C ends the client, not the container process; the xargs loaders
// would go on for minutes and collide with a rerun). Each process is
// stopped before its children are listed, so xargs cannot start another
// loader meanwhile, and killed after them. The pid file names the script;
// its cmdline is checked so a pid reused after a container restart is left
// alone. Prints a line when something was stopped. The pid file and every
// work dir are removed either way: a killed script runs no EXIT trap.
const killImportScript = `
killtree() {
	local c
	kill -STOP "$1" 2>/dev/null
	for c in $(pgrep -P "$1" 2>/dev/null || ps -o pid= --ppid "$1" 2>/dev/null); do killtree "$c"; done
	kill -KILL "$1" 2>/dev/null
}
pid=$(cat ` + importDir + `.pid 2>/dev/null)
if [ -n "$pid" ] && [ "$pid" != "$$" ] && grep -qa dockware-clone-import "/proc/$pid/cmdline" 2>/dev/null; then
	killtree "$pid"
	echo "stopped a leftover import (pid $pid)"
fi
rm -rf ` + importDir + `.*
`

// Import loads the dump into a fresh local database in two phases: all DDL
// serially (seconds), then the table data in parts loaded by up to
// importWorkers mysql clients at once with foreign_key_checks and
// unique_checks off, as the dump's own header has them. The InnoDB redo log
// is disabled for the data phase where the server allows it (MySQL ≥
// 8.0.21 as root): the database is a throwaway and the import runs three
// times faster without it. A disabled redo log loses the whole instance on
// a crash, so it is re-enabled on every path. Loaders a cancelled run left
// in the container are stopped before the database is dropped and when
// this run's import exec fails.
func (l *Local) Import(ctx context.Context, dump *DumpResult, log io.Writer) error {
	l.tuneMySQL(ctx, log)
	l.killImport(ctx, log)
	var out bytes.Buffer
	if err := l.Exec(ctx, localMySQL+" -e 'DROP DATABASE IF EXISTS "+localDB+"; CREATE DATABASE "+localDB+" CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci'", nil, io.Discard, &out); err != nil {
		return Fail(StepImport, "creating the local database failed", err, tailLines(out.String(), 20))
	}
	f, err := os.Open(dump.File)
	if err != nil {
		return Fail(StepImport, "cannot open the dump", err, "")
	}
	defer f.Close()
	decompress := "zstd -dc"
	if dump.Compression == "gzip" {
		decompress = "gzip -dc"
	}
	if err := l.Exec(ctx, localMySQL+" -e 'ALTER INSTANCE DISABLE INNODB REDO_LOG'", nil, io.Discard, io.Discard); err == nil {
		fmt.Fprintln(log, "  import: redo log disabled during import")
		defer l.enableRedoLog(log)
	} else {
		// MariaDB, MySQL before 8.0.21 or a root without INNODB_REDO_LOG_ENABLE
		fmt.Fprintln(log, "  import: redo log kept (not supported)")
	}
	out.Reset()
	if err := l.Exec(ctx, importScript(decompress, importWorkers, importPartBytes), f, &out, &out); err != nil {
		// before the deferred enableRedoLog, with its own context: ctx may
		// be the reason the exec client died while the loaders live on
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		l.killImport(bg, log)
		cancel()
		return Fail(StepImport, "importing the dump into the local database failed", err, tailLines(out.String(), 20))
	}
	return nil
}

// killImport runs killImportScript in the container and logs what it
// stopped; a failed exec is logged, the next step reports a broken docker.
func (l *Local) killImport(ctx context.Context, log io.Writer) {
	var out bytes.Buffer
	if err := l.Exec(ctx, killImportScript, nil, &out, io.Discard); err != nil {
		fmt.Fprintf(log, "  import: stopping a leftover import failed: %v\n", err)
		return
	}
	if s := strings.TrimSpace(out.String()); s != "" {
		fmt.Fprintf(log, "  import: %s\n", s)
	}
}

// enableRedoLog turns the redo log back on after Import with its own
// context: a cancelled run must not leave the instance crash-unsafe.
func (l *Local) enableRedoLog(log io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var out bytes.Buffer
	if err := l.Exec(ctx, localMySQL+" -e 'ALTER INSTANCE ENABLE INNODB REDO_LOG'", nil, io.Discard, &out); err != nil {
		fmt.Fprintf(log, "  import: re-enabling the redo log failed: %s\n", tailLines(out.String(), 1))
	}
}

// DomainMap rewrites every remote domain to a local URL, keeping paths.
// The APP_URL host maps to localhost, other hosts to <slug>.localhost.
// Local URLs drop the scheme, so when http and https variants of a domain
// both exist only the first one (in slice order) is mapped; the other keeps
// its remote URL, since sales_channel_domain.url is unique.
func DomainMap(domains []Domain, appURL string, port int) map[string]string {
	primary := ""
	if u, err := url.Parse(appURL); err == nil {
		primary = u.Host
	}
	out := map[string]string{}
	taken := map[string]bool{}
	for _, d := range domains {
		if d.URL == "" {
			continue
		}
		u, err := url.Parse(d.URL)
		if err != nil || u.Host == "" {
			// headless placeholder domains like "default.headless0" stay as they are
			continue
		}
		host := "localhost"
		if u.Host != primary {
			host = strings.NewReplacer(".", "-", ":", "-").Replace(u.Host) + ".localhost"
		}
		local := fmt.Sprintf("http://%s:%d%s", host, port, strings.TrimRight(u.Path, "/"))
		if taken[local] {
			continue
		}
		taken[local] = true
		out[d.URL] = local
	}
	return out
}

func sqlString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// PostImport makes the shop runnable locally: domains, admin user, staging
// mode, plugin and theme state, caches.
func (l *Local) PostImport(ctx context.Context, p *Probe, domains map[string]string, postSQL string, log io.Writer) error {
	var sql strings.Builder
	for _, from := range sortedKeys(domains) {
		fmt.Fprintf(&sql, "UPDATE sales_channel_domain SET url = %s WHERE url = %s;\n", sqlString(domains[from]), sqlString(from))
	}
	if _, err := l.SQL(ctx, sql.String()); err != nil {
		return Fail(StepPostImport, "rewriting the sales channel domains failed", err, "")
	}
	if postSQL != "" {
		data, err := os.ReadFile(postSQL)
		if err != nil {
			return Fail(StepPostImport, "cannot read post_sql "+postSQL, err, "")
		}
		if _, err := l.SQL(ctx, string(data)); err != nil {
			return Fail(StepPostImport, "post_sql "+postSQL+" failed", err, "")
		}
	}
	// the server's install.lock is not synced; an index.php from before 6.6 redirects every request to the web installer without it
	if err := l.Exec(ctx, "test -e install.lock || touch install.lock", nil, io.Discard, io.Discard); err != nil {
		return Fail(StepPostImport, "cannot create install.lock", err, "")
	}
	list, _ := l.Console(ctx, "list", "system", "--raw")
	var steps [][]string
	// config/jwt stays on the server (CodeExcludes) and a checkout ignores it;
	// before 6.7 the Admin and Store API sign with these files, so a clone
	// without them answers every request with 500. 6.7 signs with APP_SECRET
	// and has no such command.
	if strings.Contains(list, "system:generate-jwt-secret") {
		if _, err := os.Stat(filepath.Join(l.CodeDir, "config", "jwt", "private.pem")); os.IsNotExist(err) {
			steps = append(steps, []string{"system:generate-jwt-secret", "-n"})
		}
	}
	steps = append(steps, []string{"plugin:refresh"})
	if strings.Contains(list, "system:setup:staging") {
		steps = append(steps, []string{"system:setup:staging", "--force", "-n"})
	} else if _, err := l.SQL(ctx, "DELETE FROM system_config WHERE configuration_key = 'core.mailerSettings.disableDelivery' AND sales_channel_id IS NULL;\nINSERT INTO system_config (id, configuration_key, configuration_value, created_at) VALUES (UNHEX(REPLACE(UUID(),'-','')), 'core.mailerSettings.disableDelivery', '{\"_value\": true}', NOW(3));"); err != nil {
		return Fail(StepPostImport, "disabling mail delivery failed", err, "")
	}
	steps = append(steps,
		[]string{"user:create", "admin", "--admin", "--password=shopware", "--email=admin@example.com", "--firstName=Admin", "--lastName=Local", "-n"},
		[]string{"assets:install"},
	)
	if p.HasStorefront {
		steps = append(steps, []string{"theme:compile", "--active-only"})
	}
	steps = append(steps, []string{"cache:clear"})
	for _, s := range steps {
		fmt.Fprintf(log, "  bin/console %s\n", strings.Join(s, " "))
		if out, err := l.Console(ctx, s...); err != nil {
			if s[0] == "user:create" && strings.Contains(out, "already") {
				continue
			}
			if s[0] == "theme:compile" {
				retried, ok := l.retryThemeCompile(ctx, out, log)
				if ok {
					continue
				}
				if retried != "" {
					out = retried
				}
			}
			return &Error{Step: StepPostImport, Msg: "bin/console " + strings.Join(s, " ") + " failed",
				Hint: hintForErr(StepPostImport, err, out), Output: headTailLines(out, 12, 12), Err: err}
		}
	}
	return nil
}

// retryThemeCompile handles three theme:compile failures: Shopware before 6.5
// has no --active-only, a static theme config (StaticFileAvailableThemeProvider)
// needs files/theme-config, which theme:dump writes and the dump does not carry,
// and a fresh checkout lacks the storefront JS that the deploy builds (dist/ is
// usually gitignored); bin/build-storefront.sh builds it and compiles the themes.
func (l *Local) retryThemeCompile(ctx context.Context, out string, log io.Writer) (string, bool) {
	args := []string{"theme:compile", "--active-only"}
	// Symfony wraps error boxes at the terminal width, also inside words
	flat := strings.Join(strings.Fields(out), "")
	switch {
	case strings.Contains(flat, "forgettobuildthetheme"):
		fmt.Fprintln(log, "  storefront JS is not built in the code: bin/build-storefront.sh")
		var built bytes.Buffer
		err := l.Exec(ctx, "test -x bin/build-storefront.sh && bin/build-storefront.sh", nil, &built, &built)
		return built.String(), err == nil
	case strings.Contains(out, "theme:dump"):
		if dumpOut, err := l.Console(ctx, "theme:dump", "-n"); err != nil {
			return dumpOut, false
		}
	case strings.Contains(flat, `"--active-only"optiondoesnotexist`):
		args = args[:1]
	default:
		return "", false
	}
	retried, err := l.Console(ctx, args...)
	return retried, err == nil
}

// copyInto streams hostDir into dir (relative to the project root) of the
// shop container, skipping the given top-level entries.
func (l *Local) copyInto(ctx context.Context, hostDir, dir string, log io.Writer, exclude ...string) error {
	args := []string{"-C", hostDir, "--no-xattrs", "--exclude", ".DS_Store"}
	for _, e := range exclude {
		args = append(args, "--exclude", "./"+e)
	}
	tar := exec.CommandContext(ctx, "tar", append(args, "-cf", "-", ".")...)
	tar.Env = append(os.Environ(), "COPYFILE_DISABLE=1") // no macOS ._ files
	pipe, err := tar.StdoutPipe()
	if err != nil {
		return Fail(StepDocker, "cannot start tar for "+hostDir, err, "")
	}
	if err := tar.Start(); err != nil {
		return Fail(StepDocker, "cannot start tar for "+hostDir, err, "")
	}
	var errb bytes.Buffer
	if err := l.Exec(ctx, "mkdir -p "+ShellQuote(dir)+" && tar -C "+ShellQuote(dir)+" -xf -", pipe, log, &errb); err != nil {
		_ = pipe.Close()
		_ = tar.Wait()
		return Fail(StepDocker, "copying "+hostDir+" into "+l.container()+":"+dir+" failed", err, tailLines(errb.String(), 20))
	}
	if err := tar.Wait(); err != nil {
		return Fail(StepDocker, "reading "+hostDir+" failed", err, "")
	}
	return nil
}

// SeedVar copies the checkout's var/ into the var volume. Shopware writes
// theme and plugin state there, which projects partly track in git; the
// volume keeps those writes out of the developer's working tree.
func (l *Local) SeedVar(ctx context.Context, log io.Writer) error {
	if l.OwnsCode {
		return nil
	}
	// a fresh named volume is root-owned; the shop runs as www-data
	if out, err := l.execRoot(ctx, "chown www-data:www-data "+containerRoot+"/var"); err != nil {
		return Fail(StepDocker, "preparing the var volume of "+l.container()+" failed", err, tailLines(out, 20))
	}
	hostVar := filepath.Join(l.CodeDir, "var")
	if _, err := os.Stat(hostVar); err != nil {
		return nil
	}
	return l.copyInto(ctx, hostVar, "var", log, "cache", "log")
}
