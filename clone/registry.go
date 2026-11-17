package clone

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Registry events; the latest event per output directory is its state.
const (
	EventClone = "clone"
	EventWipe  = "wipe" // the clone's stack and dump were removed, its output directory remains
	EventPurge = "purge"
)

// registryEnv overrides the registry file, e.g. for tests.
const registryEnv = "DOCKWARE_CLONE_REGISTRY"

// maxRegistryError caps RegistryEntry.Error: the error carries tool output,
// and the registry is read line by line.
const maxRegistryError = 2 << 10

// maxRegistryLine is the longest registry line read; longer lines are no
// entry this tool wrote and are skipped.
const maxRegistryLine = 256 << 10

// RegistryEntry is one line of the append-only clone registry: which clone
// holds data where, so expired clones can be found and purged.
type RegistryEntry struct {
	Event        string    `json:"event"`
	Time         time.Time `json:"time"`
	User         string    `json:"user,omitempty"`
	Host         string    `json:"host,omitempty"`
	Name         string    `json:"name"`
	OutDir       string    `json:"out_dir"`
	Target       string    `json:"target,omitempty"`
	Root         string    `json:"root,omitempty"`
	Database     string    `json:"database,omitempty"`
	ResolvedHost string    `json:"resolved_host,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	Verified     bool      `json:"verified"`
	Error        string    `json:"error,omitempty"`
}

// CloneInfo is the latest state of a registered clone.
type CloneInfo struct {
	RegistryEntry
	Wiped   bool // the stack and dump were removed (failed verification); the output directory remains
	Expired bool // ExpiresAt has passed
	Exists  bool // OutDir still exists
}

// RegistryPath is the clone registry file: $DOCKWARE_CLONE_REGISTRY, else
// <user config dir>/dockware-cli/clones.jsonl.
func RegistryPath() (string, error) {
	if p := os.Getenv(registryEnv); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "dockware-cli", "clones.jsonl"), nil
}

func appendRegistry(e RegistryEntry) error {
	path, err := RegistryPath()
	if err != nil {
		return err
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.User == "" {
		if u, err := user.Current(); err == nil {
			e.User = u.Username
		}
	}
	if e.Host == "" {
		e.Host, _ = os.Hostname()
	}
	if len(e.Error) > maxRegistryError {
		cut := maxRegistryError
		for cut > 0 && !utf8.RuneStart(e.Error[cut]) {
			cut--
		}
		e.Error = e.Error[:cut] + " …"
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	// a line cut off by a crash would swallow this entry: start a new line
	record := append(line, '\n')
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, st.Size()-1); err != nil || last[0] != '\n' {
			record = append([]byte{'\n'}, record...)
		}
	}
	if _, err := f.Write(record); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ListClones returns the latest state per output directory of all clones
// that were not purged, oldest first; wiped clones are listed, as their
// output directory still holds the synced code and logs.
func ListClones() ([]CloneInfo, error) {
	return listClones(time.Now())
}

func listClones(now time.Time) ([]CloneInfo, error) {
	path, err := RegistryPath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	latest := map[string]RegistryEntry{}
	br := bufio.NewReaderSize(f, maxRegistryLine)
	for {
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			for err == bufio.ErrBufferFull {
				_, err = br.ReadSlice('\n')
			}
			line = nil
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
		var e RegistryEntry
		// a line cut off by a crash must not hide the other clones
		if line = bytes.TrimSpace(line); len(line) > 0 && json.Unmarshal(line, &e) == nil && e.OutDir != "" {
			latest[e.OutDir] = e
		}
		if err == io.EOF {
			break
		}
	}
	var out []CloneInfo
	for _, e := range latest {
		if e.Event != EventClone && e.Event != EventWipe {
			continue
		}
		out = append(out, CloneInfo{
			RegistryEntry: e,
			Wiped:         e.Event == EventWipe,
			Expired:       !e.ExpiresAt.IsZero() && now.After(e.ExpiresAt),
			Exists:        fileExists(e.OutDir),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Time.Equal(out[j].Time) {
			return out[i].Time.Before(out[j].Time)
		}
		return out[i].OutDir < out[j].OutDir
	})
	return out, nil
}

// UnregisteredClone describes a clone output directory missing from the
// registry, e.g. one made before the registry existed. dir needs the clone's
// clone.log.json and docker-compose.yml, whose name is the compose project.
func UnregisteredClone(dir string) (CloneInfo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return CloneInfo{}, err
	}
	if !fileExists(filepath.Join(abs, "clone.log.json")) {
		return CloneInfo{}, fmt.Errorf("%s has no clone.log.json, not a clone output directory", abs)
	}
	name := composeProjectName(abs)
	if name == "" {
		return CloneInfo{}, fmt.Errorf("%s has no docker-compose.yml with a project name", abs)
	}
	c := CloneInfo{RegistryEntry: RegistryEntry{Event: EventClone, Name: name, OutDir: abs}, Exists: true}
	if a, err := readAudit(abs); err == nil && a != nil {
		c.Time, c.Target, c.Root, c.Database, c.ResolvedHost = a.Started, a.Target, a.Root, a.Database, a.ResolvedHost
		c.ExpiresAt, c.Verified, c.Wiped = a.ExpiresAt, a.Verified, a.Wiped
	}
	return c, nil
}

// composeProjectName returns the top-level name of dir/docker-compose.yml,
// "" when the file is missing or has none.
func composeProjectName(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		return ""
	}
	var c struct {
		Name string `yaml:"name"`
	}
	if yaml.Unmarshal(data, &c) != nil {
		return ""
	}
	return strings.TrimSpace(c.Name)
}

// purgeGuard refuses output directories whose removal could take more than
// a clone with it: relative paths, /, the home directory or its parents, and
// existing directories without the clone's clone.log.json.
func purgeGuard(dir string) error {
	refuse := func(why string) error {
		return &Error{Step: StepConfig, Msg: fmt.Sprintf("refusing to purge %q: %s", dir, why),
			Hint: "remove the directory yourself after checking its content"}
	}
	if dir == "" || !filepath.IsAbs(dir) {
		return refuse("not an absolute path")
	}
	clean := filepath.Clean(dir)
	if filepath.Dir(clean) == clean {
		return refuse("filesystem root")
	}
	if home, err := os.UserHomeDir(); err == nil && insideDir(home, clean) {
		return refuse("the home directory or one of its parents")
	}
	if fileExists(clean) && !fileExists(filepath.Join(clean, "clone.log.json")) {
		return refuse("no clone.log.json, not a clone output directory")
	}
	return nil
}

// cloneFiles are the files a clone writes into its output directory (glob
// patterns); purge removes nothing else, plus code/ when the clone synced it.
var cloneFiles = []string{
	"clone.log.json", "docker-compose.yml", "shop.env", "frontend.env", "php-clone.ini",
	".dockware-clone.remote.yml", "apache-*.conf", "dump.sql*",
}

// removeCloneFiles removes what a clone wrote into dir, then dir itself when
// nothing else is left; kept lists the entries the clone did not write.
func removeCloneFiles(dir string, codeSynced bool) (kept []string, err error) {
	for _, pattern := range cloneFiles {
		matches, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
		}
	}
	if codeSynced {
		if err := os.RemoveAll(filepath.Join(dir, "code")); err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	if len(entries) > 0 {
		for _, e := range entries {
			kept = append(kept, e.Name())
		}
		return kept, nil
	}
	return nil, os.Remove(dir)
}

// Purge removes a clone: its containers and volumes, the files it wrote into
// its output directory (code it synced, dump, logs) and the directory when
// nothing else is in it, and records a purge event in the registry.
func Purge(ctx context.Context, c CloneInfo, log io.Writer) error {
	if err := purgeGuard(c.OutDir); err != nil {
		return err
	}
	dir := filepath.Clean(c.OutDir)
	if fileExists(dir) {
		fmt.Fprintf(log, "==> purging %s (%s)\n", c.Name, dir)
		a, _ := readAudit(dir)
		l := &Local{Name: c.Name, Dir: dir}
		if err := l.Down(ctx, true); err != nil {
			return err
		}
		kept, err := removeCloneFiles(dir, a != nil && a.CodeSynced)
		if err != nil {
			return Fail(StepDocker, "cannot remove the clone files in "+dir, err, "")
		}
		if len(kept) > 0 {
			fmt.Fprintf(log, "  kept %s: it holds entries the clone did not write: %s\n", dir, strings.Join(kept, ", "))
		}
	} else {
		fmt.Fprintf(log, "==> %s: %s is gone, removing it from the registry\n", c.Name, dir)
	}
	if err := appendRegistry(RegistryEntry{Event: EventPurge, Name: c.Name, OutDir: c.OutDir, Target: c.Target, Root: c.Root,
		Database: c.Database, ResolvedHost: c.ResolvedHost, ExpiresAt: c.ExpiresAt}); err != nil {
		return Fail(StepConfig, "cannot record the purge in the clone registry", err, "")
	}
	return nil
}
