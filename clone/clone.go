// Package clone copies a running Shopware shop into a local dockware
// environment with anonymised data, without loading the source server.
package clone

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Options configure one clone run.
type Options struct {
	Target          string
	Root            string
	SSHPort         int
	Identity        string
	SSHOptions      []string
	Strategy        string // stream | shadow
	ShadowDB        string
	Limits          []string
	CodeDir         string   // existing local checkout; empty = rsync from the server
	Exclude         []string // extra rsync exclude patterns for the code sync
	MaxCodeSizeMB   *int     // code sync limit in MB, -1 = no limit; nil = config max_code_size_mb, else defaultMaxCodeSizeMB
	OrdersSinceDays *int     // keep only orders of the last N days, 0 = all; nil = config orders_since_days
	SkipSearchIndex *bool    // transfer the product search index schema-only and rebuild it locally; nil = config skip_search_index
	PHP             string   // PHP major.minor of the local shop; empty = config or server
	Node            string   // node major of the local shop; empty = config, checkout, server
	ComposerAuth    string   // auth.json merged on top of FindComposerAuth's other sources
	ConfigPath      string   // default <CodeDir>/.dockware-clone.yml
	Name            string
	OutDir          string
	Port            int
	FrontendPort    int
	SWCLIVersion    string
	AppEnv          string
	Image           string
	VerifySample    int
	DumpOnly        bool
	NoFrontend      bool
	KeepUnverified  bool   // keep a clone whose anonymisation checks failed instead of removing it
	KeepDump        bool   // keep the dump file after a successful import
	ReuseDump       bool   // import the dump of the previous run in OutDir when target, root and rules hash match
	Force           bool   // reuse an OutDir that holds a clone of another target or root
	RetentionDays   int    // days until the clone counts as expired; 0 = config retention_days, else defaultRetentionDays
	SSHAgent        bool   // mount the host's ssh-agent for composer even when the checkout does not need it
	Bind            string // host address the local ports are published on; "" = 127.0.0.1
	Log             io.Writer
}

// defaultMaxCodeSizeMB caps the code sync, so a project root full of backups
// and uploads is noticed before it lands on the laptop.
const defaultMaxCodeSizeMB = 300

// defaultRetentionDays is how long a clone lives before it counts as expired
// and `dockware-cli clone purge --expired` removes it.
const defaultRetentionDays = 30

// searchIndexTables hold the product search index; Shopware rebuilds it from
// the product data (dal:refresh:index --only=product.indexer).
var searchIndexTables = []string{"product_search_keyword", "product_keyword_dictionary"}

// syncWriter serialises the shared log: the dump streams shopware-cli
// progress from its own goroutine while the local steps write tool output.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// Run executes probe → dump, overlapped with the local environment → import
// → verification → smoke tests. Every error it returns is an *Error naming
// the step that failed.
func Run(ctx context.Context, o Options) (err error) {
	log := &syncWriter{w: o.Log}
	step := StepConfig
	classify := func(err error) error {
		if err == nil {
			return nil
		}
		return Fail(step, "clone failed during "+step, err, "")
	}
	defer func() { err = classify(err) }()

	// everything that can be checked locally fails before the server is touched
	r, err := ParseTarget(o.Target)
	if err != nil {
		return Fail(StepConfig, "invalid ssh target "+o.Target, err, "")
	}
	if o.Strategy != "stream" && o.Strategy != "shadow" {
		return &Error{Step: StepConfig, Msg: fmt.Sprintf("unknown strategy %q", o.Strategy), Hint: "pass --strategy stream (default) or --strategy shadow"}
	}
	if o.PHP != "" && !configPHPVersion.MatchString(o.PHP) {
		return &Error{Step: StepConfig, Msg: fmt.Sprintf("invalid PHP version %q", o.PHP), Hint: "pass --php as major.minor, e.g. 8.3"}
	}
	if o.Node != "" && !configNodeVersion.MatchString(o.Node) {
		return &Error{Step: StepConfig, Msg: fmt.Sprintf("invalid node version %q", o.Node), Hint: "pass --node as major version, e.g. 22"}
	}
	if o.MaxCodeSizeMB != nil && (*o.MaxCodeSizeMB < -1 || *o.MaxCodeSizeMB == 0) {
		return &Error{Step: StepConfig, Msg: fmt.Sprintf("--max-code-size must be -1 (no limit) or a size in MB, got %d", *o.MaxCodeSizeMB),
			Hint: "pass the limit in MB, e.g. --max-code-size 500, or -1 for no limit"}
	}
	if o.OrdersSinceDays != nil && *o.OrdersSinceDays < 0 {
		return &Error{Step: StepConfig, Msg: fmt.Sprintf("--orders-since-days must be 0 (all orders) or more, got %d", *o.OrdersSinceDays),
			Hint: "pass the number of days whose orders are kept, or 0 for all orders"}
	}
	if o.RetentionDays < 0 {
		return &Error{Step: StepConfig, Msg: fmt.Sprintf("--retention-days must be 0 (config, else %d) or more, got %d", defaultRetentionDays, o.RetentionDays),
			Hint: "pass the number of days the clone may live, e.g. --retention-days 14"}
	}
	if o.DumpOnly && o.ReuseDump {
		return &Error{Step: StepConfig, Msg: "--reuse-dump cannot be combined with --dump-only", Hint: "--dump-only creates a new dump; drop one of the flags"}
	}
	if runtime.GOOS == "windows" && o.CodeDir == "" && !o.DumpOnly {
		return &Error{Step: StepConfig, Msg: "syncing the code from the server is not supported on Windows",
			Hint: "run dockware-cli clone inside WSL, or pass --code with a local checkout"}
	}
	if err := ValidateSSHOptions(o.SSHOptions); err != nil {
		return err
	}
	if o.SSHPort != 0 {
		r.Port = o.SSHPort
	}
	r.Identity, r.Root = o.Identity, o.Root
	for _, opt := range o.SSHOptions {
		r.Extra = append(r.Extra, "-o", opt)
	}
	defName := defaultName(r.Target, o.Root)
	if o.OutDir == "" {
		o.OutDir = o.Name
		if o.OutDir == "" {
			o.OutDir = defName
		}
	}
	if o.OutDir, err = filepath.Abs(o.OutDir); err != nil {
		return Fail(StepConfig, "cannot resolve the output directory", err, "")
	}
	if o.CodeDir != "" {
		if o.CodeDir, err = filepath.Abs(o.CodeDir); err != nil {
			return Fail(StepConfig, "cannot resolve the code directory", err, "")
		}
		if insideDir(o.OutDir, o.CodeDir) {
			return &Error{Step: StepConfig, Msg: fmt.Sprintf("output directory %s is inside the checkout %s", o.OutDir, o.CodeDir), Hint: "pass --out <dir> outside the checkout"}
		}
		// purge removes the output directory's clone files; a checkout inside it would sit next to them
		if insideDir(o.CodeDir, o.OutDir) {
			return &Error{Step: StepConfig, Msg: fmt.Sprintf("the checkout %s is inside the output directory %s", o.CodeDir, o.OutDir), Hint: "pass --out <dir> that does not contain the checkout"}
		}
	}
	// an output directory of another shop is not overwritten by accident
	prev, err := readAudit(o.OutDir)
	if err != nil && !o.Force {
		return &Error{Step: StepConfig, Msg: "cannot read clone.log.json of the previous run in " + o.OutDir, Err: err,
			Hint: "pass another --out, or --force to replace the directory's clone"}
	}
	if prev == nil && !o.Force && dirHasEntries(o.OutDir) {
		return &Error{Step: StepConfig, Msg: fmt.Sprintf("output directory %s exists and is not a clone output directory (no clone.log.json)", o.OutDir),
			Hint: "pass --out with a new or empty directory, or --force to clone into it anyway"}
	}
	foreign := prev != nil && (prev.Target != o.Target || path.Clean(prev.Root) != path.Clean(o.Root))
	if foreign && !o.Force {
		return &Error{Step: StepConfig, Msg: fmt.Sprintf("output directory %s holds a clone of %s:%s", o.OutDir, prev.Target, prev.Root),
			Hint: "pass another --out or --name, remove the old clone first (dockware-cli clone list / dockware-cli clone purge), or pass --force to replace it"}
	}
	// the stack of the previous run in OutDir: its name is kept, or its
	// containers and volumes are removed before the new stack replaces it
	oldName := ""
	if prev != nil {
		if oldName = prev.Name; oldName == "" {
			oldName = composeProjectName(o.OutDir)
		}
	}
	if o.Name == "" {
		o.Name = defName
		if oldName != "" && !foreign {
			o.Name = oldName
		}
	}
	downOld := oldName != "" && (oldName != o.Name || foreign)
	if err := os.MkdirAll(o.OutDir, 0o700); err != nil {
		return Fail(StepConfig, "cannot create output directory "+o.OutDir, err, "")
	}
	audit := newAudit(o.Target, o.Root, o.Strategy)
	audit.Name, audit.OutDir = o.Name, o.OutDir
	// code/ stays a clone's until purged, also when this run uses --code
	audit.CodeSynced = prev != nil && prev.CodeSynced
	// track is set before the dump starts: the run is then registered, also
	// when it fails, so its data can be found and purged
	track := false
	// dumpStarted is set before anything writes the dump file of OutDir
	dumpStarted := false
	register := func() {
		event := EventClone
		if audit.Wiped {
			event = EventWipe
		}
		if rerr := appendRegistry(RegistryEntry{Event: event, User: audit.User, Host: audit.Host, Name: o.Name, OutDir: o.OutDir,
			Target: o.Target, Root: o.Root, Database: audit.Database, ResolvedHost: audit.ResolvedHost, ExpiresAt: audit.ExpiresAt,
			Verified: audit.Verified, Error: audit.Error}); rerr != nil {
			msg := fmt.Sprintf("cannot record the clone in the registry: %v", rerr)
			audit.Warnings = append(audit.Warnings, msg)
			fmt.Fprintln(log, "WARN "+msg)
		}
	}
	defer func() {
		err = classify(err)
		if err != nil {
			audit.Error = err.Error()
			var ce *Error
			if errors.As(err, &ce) {
				audit.ErrorStep, audit.ErrorHint = ce.Step, ce.Hint
			}
		}
		// a run that ended before its dump left the previous dump untouched:
		// it stays reusable with --reuse-dump
		if !dumpStarted && prev != nil && !foreign {
			audit.RulesHash, audit.Dump, audit.OrdersCutoff = prev.RulesHash, prev.Dump, prev.OrdersCutoff
		}
		if track || audit.Wiped {
			register()
		}
		if werr := audit.write(o.OutDir); werr != nil && err == nil {
			err = Fail(step, "cannot write the audit log", werr, "")
		}
		fmt.Fprintf(log, "audit: %s\n", filepath.Join(o.OutDir, "clone.log.json"))
	}()
	record := func(name string, took time.Duration) {
		audit.Phases[name] = took
		fmt.Fprintf(log, "  %s: %s\n", name, took.Round(100*time.Millisecond))
	}
	phase := func(name string, start time.Time) { record(name, time.Since(start)) }
	warn := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		audit.Warnings = append(audit.Warnings, msg)
		fmt.Fprintln(log, "WARN "+msg)
	}

	if clones, lerr := ListClones(); lerr != nil {
		warn("cannot read the clone registry: %v", lerr)
	} else {
		var expired []string
		for _, c := range clones {
			if c.Expired && c.OutDir != o.OutDir {
				expired = append(expired, c.Name+" ("+c.OutDir+")")
			}
		}
		if len(expired) > 0 {
			warn("%d expired clones still hold shop data: %s; remove them with `dockware-cli clone purge --expired`", len(expired), strings.Join(expired, ", "))
		}
	}

	// a local project config is read before the server is touched
	if o.ConfigPath == "" && o.CodeDir != "" {
		if p := filepath.Join(o.CodeDir, ".dockware-clone.yml"); fileExists(p) {
			o.ConfigPath = p
		}
	}
	cfg, err := LoadProjectConfig(o.ConfigPath)
	if err != nil {
		return err
	}

	// docker has to work before the server is touched; the stream strategy
	// needs it even for --dump-only, as shopware-cli is extracted from its image
	if !o.DumpOnly || o.Strategy == "stream" {
		step = StepDocker
		if err := DockerAvailable(ctx); err != nil {
			return err
		}
		if err := LocalPreflight(ctx, o.CodeDir == "" && !o.DumpOnly); err != nil {
			return err
		}
	}

	// one ssh connection for the whole run; registered before the dump and
	// vendor joins, so it is closed after them (defers run last-in first-out)
	closeSSH, err := r.ShareConnection()
	if err != nil {
		return Fail(StepSSH, "cannot create the ssh control socket directory", err, "")
	}
	defer closeSSH()
	audit.ResolvedHost = r.ResolvedHost(ctx)

	// without a local config the project's config on the server applies
	step = StepConfig
	if o.ConfigPath == "" {
		data, found, err := r.ReadRemoteConfig(ctx)
		if err != nil {
			return err
		}
		if found {
			remoteCfg := filepath.Join(o.OutDir, ".dockware-clone.remote.yml")
			if err := os.WriteFile(remoteCfg, data, 0o600); err != nil {
				return Fail(StepConfig, "cannot write "+remoteCfg, err, "")
			}
			if cfg, err = LoadProjectConfig(remoteCfg); err != nil {
				return err
			}
			o.ConfigPath = remoteCfg
			fmt.Fprintf(log, "  config: from server (%s)\n", remoteCfg)
		}
	} else {
		fmt.Fprintf(log, "  config: %s\n", o.ConfigPath)
		if _, found, err := r.ReadRemoteConfig(ctx); err == nil && found {
			warn("the server has its own %s/.dockware-clone.yml; the local %s is used instead", strings.TrimRight(o.Root, "/"), o.ConfigPath)
		}
	}

	// effective settings: an explicitly passed flag wins over the config,
	// also when it switches a config value off
	exclude := append(append([]string(nil), cfg.Exclude...), o.Exclude...)
	maxMB := cfg.MaxCodeSizeMB
	if maxMB == 0 {
		maxMB = defaultMaxCodeSizeMB
	}
	if o.MaxCodeSizeMB != nil {
		maxMB = *o.MaxCodeSizeMB
	}
	var maxBytes int64
	if maxMB > 0 {
		maxBytes = int64(maxMB) * 1000 * 1000
	}
	days := cfg.OrdersSinceDays
	if o.OrdersSinceDays != nil {
		days = *o.OrdersSinceDays
	}
	skipIndex := cfg.SkipSearchIndex
	if o.SkipSearchIndex != nil {
		skipIndex = *o.SkipSearchIndex
	}
	retention := o.RetentionDays
	if retention == 0 {
		retention = cfg.RetentionDays
	}
	if retention == 0 {
		retention = defaultRetentionDays
	}
	audit.ExpiresAt = audit.Started.AddDate(0, 0, retention)
	audit.OrdersSinceDays, audit.SkipSearchIndex, audit.CodeExclude = days, skipIndex, exclude

	// 1. probe
	step = StepProbe
	fmt.Fprintf(log, "==> probing %s:%s\n", r.Target, r.Root)
	t := time.Now()
	p, err := r.RunProbe(ctx)
	if err != nil {
		return err
	}
	phase("probe", t)
	audit.ShopwareVersion, audit.PHPVersion, audit.MySQLVersion = p.ShopwareVersion, p.PHPVersion, p.MySQLVersion
	audit.Database, audit.DBHost, audit.AppURL = p.Database, p.DBHost, p.AppURL
	fmt.Fprintf(log, "  Shopware %s, PHP %s, MySQL %s, %d tables, %d core tables known\n",
		p.ShopwareVersion, p.PHPVersion, p.MySQLVersion, len(p.Tables), len(p.CoreTables))
	if m := EngineMismatch(p.MySQLVersion); m != "" {
		warn("%s", m)
	}

	// 2. classify and plan
	step = StepConfig
	allow := append(append([]string(nil), BuiltinAllowedTables...), cfg.AllowTables...)
	noData := append(NoDataTables(), cfg.NoData...)
	if skipIndex {
		noData = append(noData, searchIndexTables...)
	}
	c, err := Classify(p.Tables, p.CoreTables, allow, noData)
	if err != nil {
		return err
	}
	rules, err := EffectiveRules(p.Schema, cfg)
	if err != nil {
		return err
	}
	unruled := UnruledTextColumns(p.Schema, c.Allowed, rules)
	unruledTables := make([]string, 0, len(unruled))
	for tbl := range unruled {
		unruledTables = append(unruledTables, tbl)
	}
	sort.Strings(unruledTables)
	for _, tbl := range unruledTables {
		warn("allowed table %s exports text columns without a rule unchanged: %s; add rules in .dockware-clone.yml if they hold personal data",
			tbl, strings.Join(unruled[tbl], ", "))
	}
	plan := NewDumpPlan(p, c, rules, o.Limits)
	var cutoff string
	audit.RulesHash, cutoff = applyOrderWindow(&plan, p, days, time.Now())
	if days > 0 {
		if cutoff == "" {
			warn("orders_since_days ignored: the source has no order.order_date_time column; all orders are exported")
			audit.OrdersSinceDays = 0
		} else {
			fmt.Fprintf(log, "  orders: last %d days (since %s UTC)\n", days, cutoff)
		}
	}
	if skipIndex {
		fmt.Fprintln(log, "  search index: schema-only, rebuilt locally")
	}
	audit.Tables = c
	for _, cols := range plan.Rewrites {
		audit.Rewrites += len(cols)
	}
	if len(c.Denied) > 0 {
		audit.DeniedRows = map[string]int64{}
		for _, tbl := range c.Denied {
			audit.DeniedRows[tbl] = p.TableRows[tbl]
		}
		fmt.Fprintf(log, "  schema-only (not allowed, add to allow_tables if needed): %s\n", formatRows(audit.DeniedRows))
	}
	fmt.Fprintf(log, "  %d core, %d allowed, %d denied, %d no-data tables; %d rewritten columns; rules %s\n",
		len(c.Core), len(c.Allowed), len(c.Denied), len(c.NoData), audit.Rewrites, audit.RulesHash)
	var reused *DumpResult
	if o.ReuseDump {
		if reused, err = reusableDump(prev, o.OutDir, o.Target, o.Root, audit.RulesHash); err != nil {
			return err
		}
		// the reused dump holds the orders since the cutoff of its own run
		cutoff = prev.OrdersCutoff
	}
	audit.OrdersCutoff = cutoff

	// 3. dump
	dumpStep := StepDump
	if o.Strategy == "shadow" {
		dumpStep = StepShadow
	} else {
		audit.ShopwareCLIVersion = o.SWCLIVersion
	}
	runDump := func(ctx context.Context) (*DumpResult, error) {
		if o.Strategy == "shadow" {
			return ShadowDump(ctx, r, p, c, plan, o.ShadowDB, filepath.Join(o.OutDir, "dump.sql.gz"), log)
		}
		return StreamDump(ctx, r, p, plan, o.SWCLIVersion, filepath.Join(o.OutDir, "dump.sql.zst"), log)
	}
	recordDump := func(d *DumpResult, took time.Duration) {
		audit.Dump = d
		record("dump", took)
		fmt.Fprintf(log, "  %s, %.1f MB\n", filepath.Base(d.File), float64(d.Bytes)/1e6)
	}
	if reused != nil {
		fmt.Fprintf(log, "==> reusing dump %s (rules %s)\n", reused.File, audit.RulesHash)
		runDump = func(context.Context) (*DumpResult, error) { return reused, nil }
	} else {
		fmt.Fprintf(log, "==> dumping (%s)\n", o.Strategy)
	}
	// from here local data exists or is being written: registered before the
	// dump starts, so a crash or kill still leaves it findable for purge
	track = true
	register()
	dumpStarted = reused == nil
	if o.DumpOnly {
		step = dumpStep
		t = time.Now()
		dump, err := runDump(ctx)
		if err != nil {
			return err
		}
		recordDump(dump, time.Since(t))
		warn("the dump was not verified against the server (--dump-only): it must not be shared")
		return nil
	}

	// the dump runs on the server while the local environment is prepared
	// (code, container, PHP extensions): the local steps only need the probe
	// and the plan, and the dump is the longest step. The goroutine touches
	// nothing but its own locals, log and cancelLocal; its result is recorded
	// after the join. A failure on either side cancels the other
	localCtx, cancelLocal := context.WithCancel(ctx)
	defer cancelLocal()
	dumpCtx, cancelDump := context.WithCancel(ctx)
	var (
		dump     *DumpResult
		dumpErr  error
		dumpTook time.Duration
		dumpDone = make(chan struct{})
	)
	go func() {
		s := time.Now()
		dump, dumpErr = runDump(dumpCtx)
		dumpTook = time.Since(s)
		// close before the cancel: a local step that then fails on the
		// cancel finds dumpDone closed and reports the dump error
		close(dumpDone)
		if dumpErr != nil {
			cancelLocal()
		}
	}()
	// a cancel fails the dump too; it must not read as a dump failure
	dumpFailed := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return Fail(dumpStep, "clone failed during "+dumpStep, dumpErr, "")
	}
	// localFail reports a failed local step; a dump that failed first is the
	// cause, the step's "context canceled" only the effect
	localFail := func(err error) error {
		select {
		case <-dumpDone:
			if dumpErr != nil {
				return dumpFailed()
			}
		default:
		}
		return err
	}
	// a failure of the local path cancels the dump and waits for it, so the
	// ssh process is gone and the dump's own cleanup (remote temp dir,
	// shadow schema) has run before Run returns; a dump that already
	// finished still lands in the audit
	defer func() {
		cancelDump()
		<-dumpDone
		if dumpErr == nil && audit.Dump == nil {
			recordDump(dump, dumpTook)
		}
	}()

	// 4. code
	php, phpSource := p.PHPVersion, "server"
	if cfg.PHP != "" {
		php, phpSource = cfg.PHP, "config"
	}
	if o.PHP != "" {
		php, phpSource = o.PHP, "--php"
	}
	l := &Local{Name: o.Name, Dir: o.OutDir, Image: o.Image, AppEnv: o.AppEnv, PHPVersion: php, Bind: o.Bind,
		Labels: map[string]string{
			"dockware.clone.source":  o.Target + ":" + o.Root,
			"dockware.clone.created": audit.Started.UTC().Format(time.RFC3339),
			"dockware.clone.expires": audit.ExpiresAt.UTC().Format(time.RFC3339),
		},
		Warn: func(msg string) { warn("%s", msg) },
	}
	t = time.Now()
	if o.CodeDir != "" {
		l.CodeDir = o.CodeDir
		if len(exclude) > 0 {
			warn("exclude patterns only apply to the code sync from the server; --code uses the checkout as it is")
		}
		if remoteHash, err := r.Output(localCtx, "cd "+ShellQuote(r.Root)+" && php -r 'echo hash_file(\"sha256\", \"composer.lock\");'"); err == nil {
			if local, err := fileSHA256(filepath.Join(l.CodeDir, "composer.lock")); err == nil && local != strings.TrimSpace(remoteHash) {
				warn("local composer.lock differs from the server: plugin versions and migrations may not match the cloned database")
			}
		}
	} else {
		step = StepRsync
		l.CodeDir, l.OwnsCode = filepath.Join(o.OutDir, "code"), true
		audit.CodeSynced = true
		fmt.Fprintln(log, "==> syncing code")
		if err := SyncCode(localCtx, r, l.CodeDir, CodeSyncOptions{Exclude: exclude, MaxBytes: maxBytes}, log); err != nil {
			return localFail(err)
		}
	}
	phase("code", t)
	node, nodeSource := o.Node, "--node"
	if node == "" {
		node, nodeSource = cfg.Node, "config"
	}
	// an explicit version also decides the frontend image, before any detection
	l.NodePinned = node != ""
	if node == "" {
		node, nodeSource = DetectNodeVersion(l.CodeDir), "checkout"
	}
	if node == "" {
		node, nodeSource = p.NodeVersion, "server"
	}
	l.NodeVersion = node
	audit.NodeVersion = node
	if node == "" {
		fmt.Fprintf(log, "  PHP %s (%s), Node: container default\n", php, phpSource)
	} else {
		fmt.Fprintf(log, "  PHP %s (%s), Node %s (%s)\n", php, phpSource, node, nodeSource)
	}

	step = StepComposer
	auth, err := FindComposerAuth(o.ComposerAuth, l.CodeDir)
	if err != nil {
		return localFail(err)
	}
	if auth.Source == "" {
		fmt.Fprintln(log, "  composer auth: none found")
		if p.HasComposerAuth {
			warn("the server uses composer credentials (auth.json) for private repositories; composer install will likely fail without local credentials (--composer-auth)")
		}
	} else {
		fmt.Fprintf(log, "  composer auth: %s\n", auth.Source)
	}
	// the host's ssh-agent is mounted only while composer needs it
	l.SSHAgent = NeedsSSHAgent(l.CodeDir) || o.SSHAgent
	if l.SSHAgent {
		fmt.Fprintln(log, "  ssh agent: mounted for composer install, removed afterwards")
	}

	// 5. local environment
	step = StepDocker
	own := ProjectPorts(ctx, l.Name)
	l.Port = FreePort(o.Port, l.bindAddr(), own)
	var unset, reserved []string
	l.Env, unset, reserved = BuildEnv(p, l, cfg.Env)
	audit.UnsetEnv = unset
	if len(unset) > 0 {
		fmt.Fprintf(log, "  env keys set empty (secrets or unknown, override via env: in the config): %s\n", strings.Join(unset, ", "))
	}
	for _, k := range reserved {
		warn("env: %s is controlled by the dockware image and ignored; use --php/--node", k)
	}
	l.Redis = NeedsRedis(p)
	if ValidMediaURL(p.MediaURL) {
		l.MediaURL = p.MediaURL
	} else {
		warn("no usable media url on the source (%q): media missing locally will 404", p.MediaURL)
	}
	if cfg.Frontend != nil && !o.NoFrontend {
		l.Frontend = cfg.Frontend
		l.FrontPort = FreePort(o.FrontendPort, l.bindAddr(), own)
	}
	if downOld {
		fmt.Fprintf(log, "==> removing the previous stack %s of %s\n", oldName, o.OutDir)
		if err := (&Local{Name: oldName, Dir: o.OutDir}).Down(localCtx, true); err != nil {
			return localFail(err)
		}
	}
	if err := l.WriteFiles(); err != nil {
		return localFail(err)
	}
	// the host's ssh-agent never outlives the run in the container or the
	// compose file; runs after the vendor join (defers run last-in first-out)
	upDone := false
	defer func() {
		if !l.SSHAgent {
			return
		}
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var derr error
		if upDone {
			_, derr = l.DropSSHAgent(dctx, log)
		} else {
			// a stack that did not come up is not started again: the compose
			// file loses the agent and a created shop container is stopped
			l.SSHAgent = false
			if derr = l.WriteFiles(); derr == nil {
				derr = l.compose(dctx, StepDocker, "stop", "shop")
			}
		}
		if derr != nil {
			warn("removing the ssh-agent from %s failed (%v): run `docker compose -p %s -f %s down`", l.container(), derr, l.Name, filepath.Join(o.OutDir, "docker-compose.yml"))
			if err == nil {
				err = Fail(StepDocker, "cannot remove the ssh-agent from the shop container", derr, "")
			}
		}
	}()
	fmt.Fprintf(log, "==> starting %s (%s, PHP %s, port %d)\n", l.container(), l.Image, l.PHPVersion, l.Port)
	t = time.Now()
	if err := l.Up(localCtx); err != nil {
		return localFail(err)
	}
	upDone = true
	phase("container", t)
	if err := l.SeedVar(localCtx, log); err != nil {
		return localFail(err)
	}
	// the server's PHP extensions, before composer checks the platform and
	// the shop runs code that needs them (zstd cache compression, imagick)
	t = time.Now()
	missing, err := l.EnsurePHPExtensions(localCtx, p.PHPExtensions, log)
	if err != nil {
		return localFail(err)
	}
	for _, ext := range missing {
		warn("php extension %s exists on the server but not locally: install it in the image, code paths using it will fail", ext)
	}
	phase("php-extensions", t)

	// 6. vendor and import in parallel; the import waits for the dump
	fmt.Fprintln(log, "==> installing vendor and importing database")
	t = time.Now()
	vendorCtx, cancelVendor := context.WithCancel(localCtx)
	var (
		vendorErr     error
		vendorTook    time.Duration
		nodeInstalled string
		vendorDone    = make(chan struct{})
	)
	go func() {
		defer close(vendorDone)
		s := time.Now()
		// both carry the tail of their own output in the error; UseNode only
		// logs a warning when the image has no node and none was requested
		nodeInstalled, vendorErr = l.UseNode(vendorCtx, log)
		if vendorErr == nil {
			vendorErr = l.InstallVendor(vendorCtx, auth, io.Discard)
		}
		vendorTook = time.Since(s)
	}()
	// a failed dump or import stops the vendor install instead of waiting minutes for it
	defer func() {
		cancelVendor()
		<-vendorDone
	}()
	// a failed vendor install stops the dump instead of waiting minutes for it
	select {
	case <-dumpDone:
	case <-vendorDone:
		if vendorErr != nil {
			cancelDump()
			if err := ctx.Err(); err != nil {
				return err
			}
			return localFail(Fail(StepComposer, "vendor install failed", vendorErr, ""))
		}
		<-dumpDone
	}
	if dumpErr != nil {
		return dumpFailed()
	}
	recordDump(dump, dumpTook)
	step = StepImport
	importStart := time.Now()
	importErr := l.Import(ctx, dump, log)
	importTook := time.Since(importStart)
	if importErr != nil || ctx.Err() != nil {
		// no waiting minutes for composer after a failed import
		cancelVendor()
	}
	<-vendorDone
	audit.Phases["vendor"], audit.Phases["import"] = vendorTook, importTook
	phase("vendor+import", t)

	// from here the database volume holds imported data: every exit either
	// verified it or removes it (fail closed) unless --keep-unverified
	wipe := func() (hint string) {
		if werr := wipeLocal(l, dump.File, log); werr != nil {
			return fmt.Sprintf("removing the unverified clone failed (%v): run `docker compose -p %s -f %s down -v` and delete %s yourself. ",
				werr, l.Name, filepath.Join(o.OutDir, "docker-compose.yml"), dump.File)
		}
		audit.Wiped = true
		return ""
	}
	// a cancel fails both; it must not read as an import or composer failure
	if cerr := ctx.Err(); cerr != nil || importErr != nil {
		failure := cerr
		if failure == nil {
			failure = Fail(StepImport, "database import failed", importErr, "")
		}
		if o.KeepUnverified {
			warn("the partly imported database is kept (--keep-unverified): it is not verified, do not share it")
		} else if hint := wipe(); hint != "" {
			warn("%s", hint)
		}
		return failure
	}

	// 7. verification before anything is added locally
	step = StepVerify
	fmt.Fprintln(log, "==> verifying anonymisation")
	t = time.Now()
	checks, verifyErr := Verify(ctx, r, l, p, c, rules, o.VerifySample, cfg.ExpectedEmailColumns)
	audit.Checks = checks
	phase("verify", t)
	failedVerify := printChecks(log, checks)
	audit.Verified = verifyErr == nil && failedVerify == 0
	unverified := func(hint string) error {
		msg := fmt.Sprintf("%d anonymisation checks failed, see above", failedVerify)
		if verifyErr != nil {
			msg = "the anonymisation could not be verified"
		}
		return &Error{Step: StepVerify, Msg: msg, Hint: hint, Err: verifyErr}
	}
	if !audit.Verified && !o.KeepUnverified {
		hint := "the clone was removed (containers, database volume, dump) as it may hold personal data. Add rules for the listed columns " +
			"(rules: in .dockware-clone.yml) or drop them from keep_columns, then clone again; --keep-unverified keeps an unverified clone for debugging, never share it"
		return unverified(wipe() + hint)
	}
	if !audit.Verified {
		if err := ctx.Err(); err != nil {
			return err
		}
		warn("the anonymisation is not verified (--keep-unverified): this clone may hold personal data from the server, do not share it")
	}

	if vendorErr != nil {
		fmt.Fprintf(log, "  dump kept for a retry with --reuse-dump: %s\n", dump.File)
		return Fail(StepComposer, "vendor install failed", vendorErr, "")
	}
	// the data is in the database and verified; the dump is not kept around unasked
	if audit.Verified && !o.KeepDump {
		if err := os.Remove(dump.File); err != nil && !os.IsNotExist(err) {
			warn("cannot delete the dump %s: %v", dump.File, err)
		} else {
			fmt.Fprintf(log, "  dump deleted after import (--keep-dump keeps it)\n")
		}
	}
	audit.NodeVersion = strings.TrimSpace(nodeInstalled)
	if audit.NodeVersion != "" {
		fmt.Fprintf(log, "  node %s\n", audit.NodeVersion)
	}
	if l.SSHAgent {
		step = StepDocker
		lost, err := l.DropSSHAgent(ctx, log)
		if err != nil {
			return err
		}
		for _, ext := range lost {
			if !containsString(missing, ext) {
				warn("php extension %s is gone after recreating %s without the ssh-agent and apt could not install it again: run the clone again, code paths using it will fail", ext, l.container())
			}
		}
	}

	// 8. make it run locally
	step = StepPostImport
	fmt.Fprintln(log, "==> preparing shop")
	t = time.Now()
	domains := DomainMap(p.Domains, p.AppURL, l.Port)
	if err := l.PostImport(ctx, p, domains, cfg.PostSQLPath(), log); err != nil {
		return err
	}
	phase("post-import", t)
	if skipIndex {
		fmt.Fprintln(log, "==> rebuilding search index")
		t = time.Now()
		if out, err := l.Console(ctx, "dal:refresh:index", "--only=product.indexer"); err != nil {
			return Fail(StepPostImport, "search index rebuild failed", err, out)
		}
		phase("search-index", t)
	}
	keys, order, err := l.AccessKeys(ctx)
	if err != nil {
		return err
	}

	// 9. frontend
	if l.Frontend != nil {
		step = StepFrontend
		env, err := l.FrontendEnv(keys, order)
		if err != nil {
			return err
		}
		fmt.Fprintf(log, "==> starting frontend %s (%s) on port %d\n", l.Frontend.Path, l.FrontendImage(), l.FrontPort)
		if err := l.StartFrontend(ctx, env); err != nil {
			return err
		}
	}

	// 10. smoke tests
	step = StepSmoke
	fmt.Fprintln(log, "==> smoke tests")
	t = time.Now()
	smoke := Smoke(ctx, l, p, domains, keys, order)
	audit.Checks = append(audit.Checks, smoke...)
	phase("smoke", t)
	failedSmoke := printChecks(log, smoke)

	compose := filepath.Join(o.OutDir, "docker-compose.yml")
	if !audit.Verified {
		fmt.Fprintln(log, "\nWARN the anonymisation of this clone is NOT verified: it may hold personal data, do not share it")
	}
	fmt.Fprintf(log, "\nShop:     %s\nAdmin:    %s/admin (admin / shopware)\n", l.ShopURL(), l.ShopURL())
	for _, from := range sortedKeys(domains) {
		fmt.Fprintf(log, "Domain:   %s -> %s\n", from, domains[from])
	}
	for _, name := range order {
		fmt.Fprintf(log, "Access:   %-30s %s\n", name, keys[name])
	}
	if l.Frontend != nil {
		fmt.Fprintf(log, "Frontend: http://localhost:%d\n", l.FrontPort)
	}
	fmt.Fprintf(log, "Compose:  docker compose -f %s ...\n", compose)
	fmt.Fprintf(log, "Expires:  %s (dockware-cli clone purge %s)\n", audit.ExpiresAt.Format("2006-01-02"), o.Name)
	if !audit.Verified {
		return unverified("kept with --keep-unverified: the listed columns may still carry server values; add rules for them " +
			"(rules: in .dockware-clone.yml) or drop them from keep_columns, clone again, and do not share this clone")
	}
	if failedSmoke > 0 {
		return &Error{Step: StepSmoke, Msg: fmt.Sprintf("%d checks failed, see above", failedSmoke),
			Hint: "the shop is up but not healthy: see the failed checks above and `docker compose -f " + compose + " logs shop`"}
	}
	return nil
}

// wipeLocal removes the clone's containers and volumes and the dump file;
// the dump goes also when docker fails. A removed stack keeps no ssh-agent in
// its compose file. It runs on its own context, so a cancelled run still
// cleans up.
func wipeLocal(l *Local, dumpFile string, log io.Writer) error {
	fmt.Fprintln(log, "==> removing the unverified clone (containers, database volume, dump)")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	downErr := l.Down(ctx, true)
	if downErr == nil && l.SSHAgent {
		l.SSHAgent = false
		downErr = l.WriteFiles()
	}
	if err := os.Remove(dumpFile); err != nil && !os.IsNotExist(err) {
		if downErr != nil {
			return fmt.Errorf("%w; %v", downErr, err)
		}
		return err
	}
	return downErr
}

// dirHasEntries reports whether dir exists and is not empty.
func dirHasEntries(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	names, _ := f.Readdirnames(1)
	return len(names) > 0
}

// reusableDump returns the dump of the previous run in dir when it belongs to
// the same target, root and rules hash and is unchanged since that run.
func reusableDump(prev *Audit, dir, target, root, rulesHash string) (*DumpResult, error) {
	why := ""
	switch {
	case prev == nil:
		why = "no clone.log.json of a previous run in " + dir
	case prev.Target != target || path.Clean(prev.Root) != path.Clean(root):
		why = fmt.Sprintf("the previous run cloned %s:%s", prev.Target, prev.Root)
	case prev.RulesHash != rulesHash:
		why = fmt.Sprintf("the anonymisation plan changed (rules %s, previous dump %s)", rulesHash, prev.RulesHash)
	case prev.Dump == nil:
		why = "the previous run did not finish its dump"
	}
	if why == "" {
		name := filepath.Base(prev.Dump.File)
		file := filepath.Join(dir, name)
		st, err := os.Stat(file)
		switch {
		case name != "dump.sql.zst" && name != "dump.sql.gz":
			why = "the previous dump " + name + " is not a clone dump"
		case err != nil:
			why = "the dump of the previous run is gone (it is deleted after the import unless --keep-dump)"
		case st.Size() != prev.Dump.Bytes:
			why = fmt.Sprintf("%s changed since the previous run (%d bytes, recorded %d)", name, st.Size(), prev.Dump.Bytes)
		default:
			compression := "zstd"
			if name == "dump.sql.gz" {
				compression = "gzip"
			}
			return &DumpResult{File: file, Bytes: st.Size(), Compression: compression, Phases: map[string]time.Duration{}}, nil
		}
	}
	return nil, &Error{Step: StepConfig, Msg: "cannot reuse the dump: " + why,
		Hint: "run without --reuse-dump for a fresh dump; pass --keep-dump to keep a dump for the next --reuse-dump"}
}

// defaultName is the compose project name of a clone: host and a short hash
// of the project root, so two shops on one host do not share a stack.
func defaultName(target, root string) string {
	sum := sha256.Sum256([]byte(path.Clean(root)))
	return "dwc-" + slug(target) + "-" + hex.EncodeToString(sum[:])[:6]
}

func printChecks(log io.Writer, checks []Check) int {
	failed := 0
	for _, c := range checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
			failed++
		}
		if c.Detail != "" {
			fmt.Fprintf(log, "  %s %s: %s\n", mark, c.Name, c.Detail)
		} else {
			fmt.Fprintf(log, "  %s %s\n", mark, c.Name)
		}
	}
	return failed
}

// mergeRules appends project rules; a project rule replaces a default for the same column.
func mergeRules(defaults, extra []Rule) []Rule {
	idx := map[string]int{}
	out := append([]Rule(nil), defaults...)
	for i, r := range out {
		idx[r.Table+"."+r.Column] = i
	}
	for _, r := range extra {
		if i, ok := idx[r.Table+"."+r.Column]; ok {
			out[i] = r
			continue
		}
		idx[r.Table+"."+r.Column] = len(out)
		out = append(out, r)
	}
	return out
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// insideDir reports whether path equals dir or lies below it, following
// symlinks in the longest existing prefix of each path.
func insideDir(path, dir string) bool {
	rel, err := filepath.Rel(resolveExisting(dir), resolveExisting(path))
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveExisting evaluates symlinks in the longest existing prefix of path.
func resolveExisting(path string) string {
	rest := ""
	for p := path; ; {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return path
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

func fileSHA256(p string) (string, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func formatRows(m map[string]int64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s(~%d)", k, m[k])
	}
	return strings.Join(parts, ", ")
}

// httpClient resolves every *.localhost name to 127.0.0.1, like browsers do.
var httpClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err == nil && (host == "localhost" || strings.HasSuffix(host, ".localhost")) {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
	}},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func httpCheck(ctx context.Context, name, method, u string, headers map[string]string, body string, want int) (Check, []byte) {
	req, err := http.NewRequestWithContext(ctx, method, u, strings.NewReader(body))
	if err != nil {
		return Check{Name: name, Detail: err.Error()}, nil
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return Check{Name: name, Detail: err.Error()}, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return Check{Name: name, OK: resp.StatusCode == want, Detail: fmt.Sprintf("%s %s → %d, %d bytes", method, u, resp.StatusCode, len(data))}, data
}

// Smoke exercises storefront, Store API (headless), Admin API and frontend.
func Smoke(ctx context.Context, l *Local, p *Probe, domains map[string]string, keys map[string]string, order []string) []Check {
	var checks []Check
	if p.HasStorefront {
		for _, d := range p.Domains {
			if d.TypeID != SalesChannelTypeStorefront || domains[d.URL] == "" {
				continue
			}
			c, _ := httpCheck(ctx, "storefront "+d.Name, http.MethodGet, domains[d.URL]+"/", nil, "", http.StatusOK)
			checks = append(checks, c)
			break
		}
	}
	for _, name := range order {
		c, _ := httpCheck(ctx, "store-api "+name, http.MethodPost, l.ShopURL()+"/store-api/product", map[string]string{
			"sw-access-key": keys[name], "Content-Type": "application/json", "Accept": "application/json",
		}, `{"limit":1}`, http.StatusOK)
		checks = append(checks, c)
	}
	c, _ := httpCheck(ctx, "admin api login", http.MethodPost, l.ShopURL()+"/api/oauth/token", map[string]string{"Content-Type": "application/json"},
		`{"grant_type":"password","client_id":"administration","scopes":"write","username":"admin","password":"shopware"}`, http.StatusOK)
	checks = append(checks, c)

	if l.Frontend != nil {
		u := "http://localhost:" + fmt.Sprint(l.FrontPort) + "/"
		// dev servers compile on first request
		deadline := time.Now().Add(10 * time.Minute)
		var last Check
		for time.Now().Before(deadline) {
			last, _ = httpCheck(ctx, "frontend SSR "+l.Frontend.Path, http.MethodGet, u, nil, "", http.StatusOK)
			if last.OK {
				break
			}
			select {
			case <-ctx.Done():
				return append(checks, last)
			case <-time.After(5 * time.Second):
			}
		}
		checks = append(checks, last)
	}
	return checks
}
