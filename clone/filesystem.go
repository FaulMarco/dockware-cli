package clone

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FilesystemOverrideFile sits in config/packages/<env>/ and sorts last, so it
// wins over the project's own filesystem config.
const FilesystemOverrideFile = "zz-dockware-clone.yaml"

var localFilesystemRoots = map[string]string{
	"public":  "%kernel.project_dir%/public",
	"private": "%kernel.project_dir%/files",
	"theme":   "%kernel.project_dir%/public",
	"asset":   "%kernel.project_dir%/public",
	"sitemap": "%kernel.project_dir%/public",
}

// RemoteFilesystems returns the Shopware filesystems that config/packages and
// config/packages/<env> put on a non-local adapter (name → type, e.g.
// public → amazon-s3), in Symfony's load order. Files that are not plain YAML
// are skipped.
func RemoteFilesystems(codeDir, env string) map[string]string {
	dir := filepath.Join(codeDir, "config", "packages")
	var files []string
	for _, d := range []string{dir, filepath.Join(dir, env)} {
		var level []string
		for _, ext := range []string{"*.yaml", "*.yml"} {
			m, _ := filepath.Glob(filepath.Join(d, ext))
			level = append(level, m...)
		}
		sort.Strings(level)
		files = append(files, level...)
	}
	types := map[string]string{}
	for _, f := range files {
		if filepath.Base(f) == FilesystemOverrideFile {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var doc struct {
			Shopware struct {
				Filesystem map[string]struct {
					Type string `yaml:"type"`
				} `yaml:"filesystem"`
			} `yaml:"shopware"`
		}
		if yaml.Unmarshal(data, &doc) != nil {
			continue
		}
		for name, fs := range doc.Shopware.Filesystem {
			if _, known := localFilesystemRoots[name]; known && fs.Type != "" {
				types[name] = fs.Type
			}
		}
	}
	for name, typ := range types {
		if typ == "local" {
			delete(types, name)
		}
	}
	return types
}

// WriteFilesystemOverride points the given filesystems at local directories,
// so the clone never reaches the source's object storage (its credentials are
// not copied). Without names it removes an override from an earlier run.
func WriteFilesystemOverride(codeDir, env string, names []string) error {
	path := filepath.Join(codeDir, "config", "packages", env, FilesystemOverrideFile)
	if len(names) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("# written by dockware-cli clone: the source keeps these filesystems in object storage\nshopware:\n  filesystem:\n")
	for _, name := range names {
		b.WriteString("    " + name + ":\n      type: \"local\"\n")
		if name != "private" {
			b.WriteString("      url: \"%env(APP_URL)%\"\n")
		}
		b.WriteString("      config:\n        root: \"" + localFilesystemRoots[name] + "\"\n")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFile(path, []byte(b.String()), 0o644)
}
