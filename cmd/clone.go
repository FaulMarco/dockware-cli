package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/dockware/dockware-cli/clone"
	"github.com/spf13/cobra"
)

var cloneOpts = clone.Options{Log: os.Stdout}

// Flags whose explicit value must win over the config even when it switches
// a config setting off; they reach Options only when passed (see RunE).
var (
	cloneOrdersSinceDays int
	cloneSkipSearchIndex bool
	cloneMaxCodeSizeMB   int
)

func init() {
	f := cloneCmd.Flags()
	f.StringVar(&cloneOpts.Root, "path", "", "Shopware project root on the server (required)")
	f.IntVar(&cloneOpts.SSHPort, "ssh-port", 0, "ssh port (default: ssh config)")
	f.StringVar(&cloneOpts.Identity, "identity", "", "ssh private key (default: ssh config/agent)")
	f.StringArrayVar(&cloneOpts.SSHOptions, "ssh-option", nil, "extra ssh -o option, e.g. ConnectTimeout=20 or ProxyJump=bastion (repeatable; host key checks cannot be switched off)")
	f.StringVar(&cloneOpts.Strategy, "strategy", "stream", "stream: anonymise inside the dump query (no server writes); shadow: anonymised copy into a temporary schema, then dump")
	f.StringVar(&cloneOpts.ShadowDB, "shadow-db", "", "shadow schema name (default <db>_dwc_shadow; must not exist or be empty)")
	f.StringArrayVar(&cloneOpts.Limits, "limit", nil, "shopware-cli row limit table:rows (stream only; creates staging tables on the server)")
	f.IntVar(&cloneOrdersSinceDays, "orders-since-days", 0, "keep only orders of the last N days with their line items, deliveries, transactions and documents; 0 = all orders (default: orders_since_days in the config, else all)")
	f.BoolVar(&cloneSkipSearchIndex, "skip-search-index", false, "do not transfer product_search_keyword and product_keyword_dictionary; rebuild the search index locally (default: skip_search_index in the config; =false overrides a config true)")
	f.StringVar(&cloneOpts.CodeDir, "code", "", "use this local checkout instead of syncing the code from the server")
	f.StringArrayVar(&cloneOpts.Exclude, "exclude", nil, "extra rsync exclude pattern for the code sync, e.g. /custom/static-plugins/Big/ (repeatable; added to exclude: in the config)")
	f.IntVar(&cloneMaxCodeSizeMB, "max-code-size", 0, "abort the code sync when the project exceeds this many MB; -1 = no limit (default: max_code_size_mb in the config, else 300)")
	f.StringVar(&cloneOpts.ComposerAuth, "composer-auth", "", "auth.json for composer install; merged like composer does with COMPOSER_AUTH env, <code>/auth.json, $COMPOSER_HOME/auth.json, ~/.composer/auth.json and ~/.config/composer/auth.json (earlier sources win per host)")
	f.StringVar(&cloneOpts.PHP, "php", "", "PHP version of the local shop, e.g. 8.3 (default: php in the config, else the server's version)")
	f.StringVar(&cloneOpts.Node, "node", "", "node major version of the local shop and the default frontend image, e.g. 22 (default: node in the config, .nvmrc/.node-version/package.json of the code, else node -v on the server)")
	f.StringVar(&cloneOpts.ConfigPath, "config", "", "clone config (default <code>/.dockware-clone.yml)")
	f.StringVar(&cloneOpts.Name, "name", "", "compose project name (default dwc-<host>-<6 hex of sha256(path)>)")
	f.StringVar(&cloneOpts.OutDir, "out", "", "output directory (default ./<name>)")
	f.IntVar(&cloneOpts.Port, "port", 8000, "first local HTTP port to try")
	f.IntVar(&cloneOpts.FrontendPort, "frontend-port", 3000, "first local frontend port to try")
	f.StringVar(&cloneOpts.SWCLIVersion, "shopware-cli-version", "0.19.7", "shopware-cli image tag used for the stream dump")
	f.StringVar(&cloneOpts.AppEnv, "app-env", "prod", "APP_ENV of the local shop (dev needs dev dependencies in vendor/)")
	f.StringVar(&cloneOpts.Image, "image", "dockware/shopware-essentials:latest", "dockware image")
	f.IntVar(&cloneOpts.VerifySample, "verify-sample", 50, "rows per column compared against the server")
	f.BoolVar(&cloneOpts.DumpOnly, "dump-only", false, "stop after the anonymised dump (the stream strategy still needs Docker for shopware-cli)")
	f.BoolVar(&cloneOpts.NoFrontend, "no-frontend", false, "do not start the configured headless frontend")
	f.BoolVar(&cloneOpts.KeepUnverified, "keep-unverified", false, "keep the clone when anonymisation checks fail (default: containers, database volume and dump are removed); never share such a clone")
	f.BoolVar(&cloneOpts.KeepDump, "keep-dump", false, "keep the dump file after a successful import (default: deleted)")
	f.BoolVar(&cloneOpts.ReuseDump, "reuse-dump", false, "import the kept dump of the previous run in --out instead of dumping again, when target, path and rules hash match")
	f.BoolVar(&cloneOpts.Force, "force", false, "use --out even when it holds a clone of another target or path (whose stack is removed), or files of no clone")
	f.IntVar(&cloneOpts.RetentionDays, "retention-days", 0, "days until the clone counts as expired for 'dockware-cli clone purge --expired' (default: retention_days in the config, else 30)")
	f.BoolVar(&cloneOpts.SSHAgent, "ssh-agent", false, "mount the host's ssh-agent for composer install even when composer.json has no ssh repositories (removed after the install)")
	f.StringVar(&cloneOpts.Bind, "bind", "127.0.0.1", "host address the local shop and frontend ports listen on")
	_ = cloneCmd.MarkFlagRequired("path")
	rootCmd.AddCommand(cloneCmd)
}

var cloneCmd = &cobra.Command{
	Use:   "clone <ssh-target>",
	Short: "Clone a running Shopware shop into a local dockware environment with anonymised data",
	Long: `Clone copies a Shopware shop over SSH into a local dockware environment.

The database is anonymised on the server before it is transferred: personal
data never reaches this machine. Log, queue, cache and session tables are
exported without rows, extension tables are exported without rows unless
allowed in .dockware-clone.yml. The source is only read (stream strategy).
Without a local config, <path>/.dockware-clone.yml on the server is used.

After the import every anonymised column is compared against the server. When
a check fails, or the run fails or is cancelled after the import started, the
clone fails closed: containers, the database volume and the dump are removed
(--keep-unverified keeps them for debugging only). The dump is deleted after a
verified import unless --keep-dump.

The code comes from a local checkout (--code) or is synced from the server
without runtime data, uploads, backups and secrets. vendor/ is never copied:
composer install runs in the container with the PHP version of the server and
the node version of the project (.nvmrc, .node-version, package.json). Media
is served from the source URL instead of being copied. The shop listens on
127.0.0.1 only (--bind).

Every clone is registered with an expiry date (--retention-days, default 30).
'dockware-cli clone list' shows them, 'dockware-cli clone purge --expired' removes
expired clones with their data.

Every failure names the step it happened in and what to do about it; the
same is written to <out>/clone.log.json (error_step, error_hint). The exit
code is non-zero on any failure, including failed verification checks.

<ssh-target> is an ssh config alias, user@host or ssh://user@host:port.
See ` + clone.DocsURL + ` for the configuration reference.`,
	Example: `  dockware-cli clone shop-stage --path /var/www/shop --code ~/projects/shop
  dockware-cli clone shop-stage --path /var/www/shop --orders-since-days 90 --skip-search-index
  dockware-cli clone shop-stage --path /var/www/shop --exclude '/custom/static-plugins/Legacy*/' --max-code-size 500
  dockware-cli clone ssh://deploy@shop.example.com:2222 --path /var/www/html --strategy shadow
  dockware-cli clone shop-stage --path /var/www/shop --dump-only`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		defer stop()
		// after the first signal the default handling is back: a second Ctrl+C terminates
		go func() {
			<-ctx.Done()
			stop()
		}()
		cloneOpts.Target = args[0]
		flags := cmd.Flags()
		if flags.Changed("orders-since-days") {
			cloneOpts.OrdersSinceDays = &cloneOrdersSinceDays
		}
		if flags.Changed("skip-search-index") {
			cloneOpts.SkipSearchIndex = &cloneSkipSearchIndex
		}
		if flags.Changed("max-code-size") {
			cloneOpts.MaxCodeSizeMB = &cloneMaxCodeSizeMB
		}
		// cobra prints "Error: <err>" once to stderr and Execute exits 1;
		// the *clone.Error text already carries step, output tail and hint.
		return clone.Run(ctx, cloneOpts)
	},
}
