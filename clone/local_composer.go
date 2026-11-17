package clone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ComposerAuth is the merged content of composer's credential sources and
// where it came from.
type ComposerAuth struct {
	JSON   []byte
	Source string // sources used, highest precedence first, comma separated; "" when nothing was found
}

type composerAuthFile struct{ path, label string }

// composerAuthFiles lists the auth.json files in precedence order: the
// checkout, then composer's home directories.
func composerAuthFiles(codeDir string) []composerAuthFile {
	var files []composerAuthFile
	if codeDir != "" {
		p := filepath.Join(codeDir, "auth.json")
		files = append(files, composerAuthFile{p, p})
	}
	if home := os.Getenv("COMPOSER_HOME"); home != "" {
		files = append(files, composerAuthFile{filepath.Join(home, "auth.json"), "$COMPOSER_HOME/auth.json"})
	}
	if home, err := os.UserHomeDir(); err == nil {
		files = append(files,
			composerAuthFile{filepath.Join(home, ".composer", "auth.json"), "~/.composer/auth.json"},
			composerAuthFile{filepath.Join(home, ".config", "composer", "auth.json"), "~/.config/composer/auth.json"},
		)
	}
	return files
}

// composerAuthLabels names every place credentials are looked for, highest
// precedence first.
func composerAuthLabels(codeDir string) []string {
	labels := []string{"--composer-auth", "COMPOSER_AUTH env"}
	for _, f := range composerAuthFiles(codeDir) {
		labels = append(labels, f.label)
	}
	return labels
}

// authObject parses the shape of composer's auth.json, a JSON object of
// sections; nil for anything else.
func authObject(data []byte) map[string]json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil {
		return nil
	}
	return obj
}

// mergeAuth adds what dst lacks from src: whole sections (http-basic,
// bearer, github-oauth, …) and, within a section both have, hosts. dst
// has the higher precedence, as with composer's global and project auth.json.
func mergeAuth(dst, src map[string]json.RawMessage) {
	for section, raw := range src {
		have, ok := dst[section]
		if !ok {
			dst[section] = raw
			continue
		}
		var hosts, more map[string]json.RawMessage
		if json.Unmarshal(have, &hosts) != nil || hosts == nil || json.Unmarshal(raw, &more) != nil {
			continue // not keyed by host on both sides: the higher precedence value stays
		}
		for host, v := range more {
			if _, ok := hosts[host]; !ok {
				hosts[host] = v
			}
		}
		if merged, err := authJSON(hosts); err == nil {
			dst[section] = merged
		}
	}
}

// authJSON encodes credentials without HTML escaping, so passwords keep
// their characters as composer wrote them.
func authJSON(v map[string]json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

// FindComposerAuth merges composer's credential sources the way composer
// does, highest precedence first: the --composer-auth file, the COMPOSER_AUTH
// variable, auth.json in the checkout, $COMPOSER_HOME/auth.json,
// ~/.composer/auth.json and ~/.config/composer/auth.json. A section or host
// set by a higher source wins, the rest is added. Nothing is read from the
// server. Errors never contain file contents.
func FindComposerAuth(flagPath, codeDir string) (ComposerAuth, error) {
	var merged map[string]json.RawMessage
	var sources []string
	add := func(label string, data []byte, hint string) error {
		obj := authObject(data)
		if obj == nil {
			return &Error{Step: StepConfig, Msg: label + " is not a JSON object", Hint: hint}
		}
		if merged == nil {
			merged = obj
		} else {
			mergeAuth(merged, obj)
		}
		sources = append(sources, label)
		return nil
	}
	if flagPath != "" {
		data, err := os.ReadFile(flagPath)
		if err != nil {
			return ComposerAuth{}, &Error{Step: StepConfig, Msg: "cannot read --composer-auth " + flagPath, Err: err,
				Hint: "point --composer-auth at a readable composer auth.json, or drop the flag: the checkout's auth.json, $COMPOSER_HOME/auth.json, ~/.composer/auth.json, ~/.config/composer/auth.json and COMPOSER_AUTH are read anyway"}
		}
		if err := add("--composer-auth "+flagPath, data, "composer's auth.json is a JSON object such as {\"http-basic\": {\"packages.shopware.com\": {\"username\": …, \"password\": …}}}"); err != nil {
			return ComposerAuth{}, err
		}
	}
	if v := strings.TrimSpace(os.Getenv("COMPOSER_AUTH")); v != "" {
		if err := add("COMPOSER_AUTH env", []byte(v), "unset COMPOSER_AUTH or set it to the content of a composer auth.json"); err != nil {
			return ComposerAuth{}, err
		}
	}
	for _, f := range composerAuthFiles(codeDir) {
		data, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		if err := add(f.label, data, "fix or remove that auth.json, or point --composer-auth at a valid one"); err != nil {
			return ComposerAuth{}, err
		}
	}
	if merged == nil {
		return ComposerAuth{}, nil
	}
	data, err := authJSON(merged)
	if err != nil {
		return ComposerAuth{}, &Error{Step: StepConfig, Msg: "cannot merge composer credentials from " + strings.Join(sources, ", "), Err: err}
	}
	return ComposerAuth{JSON: data, Source: strings.Join(sources, ", ")}, nil
}

// InstallVendor runs composer install inside the container. The credentials
// reach composer through the process environment (docker exec -e NAME), so
// they never appear on a command line or in the output.
func (l *Local) InstallVendor(ctx context.Context, auth ComposerAuth, log io.Writer) error {
	args := []string{"exec", "-i", "-w", containerRoot}
	env := os.Environ()
	if auth.JSON != nil {
		args = append(args, "-e", "COMPOSER_AUTH")
		env = append(env, "COMPOSER_AUTH="+string(auth.JSON))
	}
	if l.SSHAgent && hostSSHAgent() != "" {
		args = append(args, "-e", "GIT_SSH_COMMAND=ssh -o StrictHostKeyChecking=accept-new")
	}
	args = append(args, l.container(), "bash", "-c", "composer install --no-interaction --no-scripts --prefer-dist --no-progress")
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = env
	var out bytes.Buffer
	w := io.MultiWriter(log, &out)
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	if err == nil {
		return nil
	}
	failed := Fail(StepComposer, "composer install failed", err, tailLines(out.String(), 25))
	var e *Error
	if auth.Source == "" && errors.As(failed, &e) && (e.Hint == "" || e.Hint == hintFor(StepComposer, "Authentication required")) {
		e.Msg = "composer install failed and no composer credentials were found (looked for " + strings.Join(composerAuthLabels(l.CodeDir), ", ") + ")"
	}
	return failed
}

// nvmDir is where the dockware image keeps www-data's node versions.
const nvmDir = "/var/www/.nvm"

// UseNode makes NodeVersion the default node of the shop container the way
// the dockware entrypoint does for NODE_VERSION, but installs the version
// first when the image lacks it. It returns the effective `node -v`; when
// no version was requested and the image has no node, that is "" with a
// warning on log (composer runs without scripts, so the clone goes on).
// Tool output stays out of log: a failure carries its tail in the error.
func (l *Local) UseNode(ctx context.Context, log io.Writer) (string, error) {
	major := l.NodeVersion
	var out bytes.Buffer
	if major != "" {
		if !configNodeVersion.MatchString(major) {
			return "", &Error{Step: StepConfig, Msg: "node version must be a major number such as 20, got " + strconv.Quote(major)}
		}
		nvm := "set -e\nexport NVM_DIR=" + nvmDir + "\n. \"$NVM_DIR/nvm.sh\" --no-use\n"
		if err := l.Exec(ctx, nvm+"nvm ls "+major+" >/dev/null 2>&1 || nvm install --no-progress "+major+"\nnvm alias default "+major, nil, &out, &out); err != nil {
			return "", l.nodeError(major, err, out.String())
		}
		// nvm is per user; like the entrypoint, link the default for root and non-login shells
		rootOut, err := l.execRoot(ctx, nvm+"bin=$(nvm which default)\nln -sfn \"$bin\" /usr/local/bin/node\nln -sfn \"$(dirname \"$bin\")/npm\" /usr/local/bin/npm")
		if err != nil {
			return "", l.nodeError(major, err, rootOut)
		}
		out.Reset()
	}
	if err := l.Exec(ctx, "node -v", nil, &out, &out); err != nil {
		if major == "" {
			fmt.Fprintf(log, "  warning: node is not available in %s; builds that need node will fail\n", l.container())
			return "", nil
		}
		return "", l.nodeError(major, err, out.String())
	}
	return strings.TrimSpace(out.String()), nil
}

func (l *Local) nodeError(major string, err error, out string) error {
	e := &Error{Step: StepDocker, Msg: "switching to node " + major + " failed", Output: tailLines(out, 25), Err: err}
	if e.Hint = hintFor(StepDocker, out); e.Hint == "" {
		e.Hint = "nvm could not provide node " + major + " in " + l.container() + ": check Docker's network access, or pass --node <major> / node: in .dockware-clone.yml with a version the image ships"
	}
	return e
}
