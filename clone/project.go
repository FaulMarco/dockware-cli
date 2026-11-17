package clone

import (
	"bytes"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProjectConfig is the optional .dockware-clone.yml next to the project.
type ProjectConfig struct {
	// AllowTables exports data of extension tables (glob patterns).
	AllowTables []string `yaml:"allow_tables"`
	// NoData adds schema-only tables (glob patterns), also for core tables.
	NoData []string `yaml:"no_data"`
	// Rules adds column rewrites on top of the defaults.
	Rules []Rule `yaml:"rules"`
	// KeepColumns exports these table.column entries raw although a default rule would clear them.
	KeepColumns []string `yaml:"keep_columns"`
	// ExpectedEmailColumns are table.column globs where e-mail addresses are expected (verify does not fail on them).
	ExpectedEmailColumns []string `yaml:"expected_email_columns"`
	// RetentionDays is how long a clone may be kept before it is reported as due for removal; 0 = default (30 days).
	RetentionDays int `yaml:"retention_days"`
	// Env overrides generated environment variables of the local shop.
	Env map[string]string `yaml:"env"`
	// PostSQL runs after import, e.g. sandbox payment keys (path relative to the config file).
	PostSQL string `yaml:"post_sql"`
	// Exclude adds rsync patterns the code sync skips, e.g. /public/catalogs/ or *.psd.
	Exclude []string `yaml:"exclude"`
	// MaxCodeSizeMB aborts the code sync above this size; 0 = default, -1 = no limit.
	MaxCodeSizeMB int `yaml:"max_code_size_mb"`
	// OrdersSinceDays keeps only orders of the last n days and their child rows; 0 = all orders.
	OrdersSinceDays int `yaml:"orders_since_days"`
	// SkipSearchIndex transfers the product search index schema-only (smaller dump) and rebuilds it after import.
	SkipSearchIndex bool `yaml:"skip_search_index"`
	// PHP pins the PHP version of the local shop, e.g. "8.3"; empty = the server's version.
	PHP string `yaml:"php"`
	// Node pins the Node major version of the shop container and the frontend image, e.g. "20";
	// empty = detected from the code (the frontend directory first), else the server's node.
	Node string `yaml:"node"`
	// Frontend starts a headless frontend against the local shop.
	Frontend *FrontendConfig `yaml:"frontend"`

	dir string
}

// FrontendConfig describes a headless frontend (Nuxt, Next, …) in the code tree.
// Env values may use ${SHOP_URL}, ${SHOP_URL_INTERNAL}, ${FRONTEND_URL} and
// ${ACCESS_KEY} / ${ACCESS_KEY:<sales channel name>}.
type FrontendConfig struct {
	// Path is relative to the code tree, or absolute (also ~/…) for a frontend in its own repository.
	Path string `yaml:"path"`
	// Workdir is the app directory inside Path for monorepos whose app imports files outside its own directory.
	Workdir string `yaml:"workdir"`
	// Image is the container image; empty = node:<major> from --node / node:, else detected from the version files.
	Image   string `yaml:"image"`
	Install string `yaml:"install"`
	Command string `yaml:"command"`
	Port    int    `yaml:"port"`
	// BasePath is the URL path the app serves under, e.g. /shop/; the SSR check requests it.
	BasePath string            `yaml:"base_path"`
	Env      map[string]string `yaml:"env"`
}

var (
	configPHPVersion  = regexp.MustCompile(`^\d+\.\d+$`)
	configNodeVersion = regexp.MustCompile(`^\d+$`)
)

// LoadProjectConfig reads the config file; an empty path yields an empty config.
func LoadProjectConfig(path string) (*ProjectConfig, error) {
	cfg := &ProjectConfig{}
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, configError(path, "cannot read the config file", err.Error())
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, configError(path, "not a valid config file", err.Error())
	}
	abs, _ := filepath.Abs(path)
	cfg.dir = filepath.Dir(abs)
	for _, r := range cfg.Rules {
		if _, ok := kindNames[r.Kind]; !ok {
			return nil, configError(path, fmt.Sprintf("unknown rule kind %q for %s.%s", r.Kind, r.Table, r.Column), "")
		}
	}
	for _, p := range cfg.Exclude {
		if !validExclude(p) {
			return nil, configError(path, fmt.Sprintf("invalid exclude pattern %q (one rsync pattern per entry)", p), "")
		}
	}
	for _, k := range cfg.KeepColumns {
		if strings.Count(k, ".") != 1 || strings.HasPrefix(k, ".") || strings.HasSuffix(k, ".") || strings.ContainsAny(k, "*?[ ") {
			return nil, configError(path, fmt.Sprintf("invalid keep_columns entry %q (one table.column per entry)", k), "")
		}
	}
	for _, p := range cfg.ExpectedEmailColumns {
		if _, err := pathpkg.Match(p, ""); err != nil || strings.Count(p, ".") != 1 {
			return nil, configError(path, fmt.Sprintf("invalid expected_email_columns entry %q (one table.column glob per entry, e.g. acme_*.email)", p), "")
		}
	}
	if cfg.RetentionDays < 0 {
		return nil, configError(path, fmt.Sprintf("retention_days must be 0 or more, got %d", cfg.RetentionDays), "")
	}
	if cfg.MaxCodeSizeMB < -1 {
		return nil, configError(path, fmt.Sprintf("max_code_size_mb must be -1 (no limit), 0 (default) or a size, got %d", cfg.MaxCodeSizeMB), "")
	}
	if cfg.OrdersSinceDays < 0 {
		return nil, configError(path, fmt.Sprintf("orders_since_days must be 0 (all orders) or more, got %d", cfg.OrdersSinceDays), "")
	}
	if cfg.PHP != "" && !configPHPVersion.MatchString(cfg.PHP) {
		return nil, configError(path, fmt.Sprintf("php must be major.minor like 8.3, got %q", cfg.PHP), "")
	}
	if cfg.Node != "" && !configNodeVersion.MatchString(cfg.Node) {
		return nil, configError(path, fmt.Sprintf("node must be a major version like 20, got %q", cfg.Node), "")
	}
	if f := cfg.Frontend; f != nil {
		if f.Path == "" || f.Command == "" {
			return nil, configError(path, "frontend needs path and command", "")
		}
		if strings.HasPrefix(f.Path, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, configError(path, "frontend.path: "+err.Error(), "")
			}
			f.Path = filepath.Join(home, f.Path[2:])
		}
		if w := filepath.ToSlash(f.Workdir); w != "" && (pathpkg.IsAbs(w) || pathpkg.Clean(w) == ".." || strings.HasPrefix(pathpkg.Clean(w), "../")) {
			return nil, configError(path, fmt.Sprintf("frontend.workdir must be a directory inside frontend.path, got %q", f.Workdir), "")
		}
		if f.Port == 0 {
			f.Port = 3000
		}
		if f.BasePath == "" {
			f.BasePath = "/"
		}
		if !strings.HasPrefix(f.BasePath, "/") {
			return nil, configError(path, fmt.Sprintf("frontend.base_path must start with /, got %q", f.BasePath), "")
		}
	}
	return cfg, nil
}

// configError names the file so the user knows which .dockware-clone.yml to fix.
func configError(path, msg, output string) error {
	return &Error{Step: StepConfig, Msg: path + ": " + msg, Output: output, Hint: "see " + DocsURL + " (configuration)"}
}

// PostSQLPath resolves post_sql relative to the config file.
func (c *ProjectConfig) PostSQLPath() string {
	if c.PostSQL == "" || filepath.IsAbs(c.PostSQL) {
		return c.PostSQL
	}
	return filepath.Join(c.dir, c.PostSQL)
}

// Dir is the frontend directory on the host.
func (f *FrontendConfig) Dir(codeDir string) string {
	if filepath.IsAbs(f.Path) {
		return f.Path
	}
	return filepath.Join(codeDir, f.Path)
}

// AppDir is the directory on the host that install and command run in.
func (f *FrontendConfig) AppDir(codeDir string) string {
	return filepath.Join(f.Dir(codeDir), f.Workdir)
}

// Name labels the frontend in the log: the app directory, below the code or below its own repository.
func (f *FrontendConfig) Name() string {
	root := f.Path
	if filepath.IsAbs(root) {
		root = filepath.Base(root)
	}
	return filepath.ToSlash(filepath.Join(root, f.Workdir))
}

var kindNames = map[Kind]bool{
	KindEmail: true, KindFirstName: true, KindLastName: true, KindCompany: true, KindStreet: true,
	KindZip: true, KindCity: true, KindPhone: true, KindIP: true, KindUsername: true, KindToken: true,
	KindPassword: true, KindEmptyJSON: true, KindNull: true,
}
