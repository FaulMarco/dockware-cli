# dockware-cli clone

`dockware-cli clone` copies a running Shopware 6 shop from a server into a
local dockware docker-compose environment. The database is anonymised on the
server before it is transferred.

```
dockware-cli clone <ssh-target> --path <project root on the server> [flags]
dockware-cli clone list
dockware-cli clone purge [name|out-dir...] [--expired] [--yes]
```

Step-by-step guide: [`clone-guide.md`](clone-guide.md).

## What it does

1. **Probe** the server over ssh: Shopware, PHP, MySQL and node versions, PHP
   extensions, the schema, core tables, sales channel domains, `.env` key
   names. No row of a personal-data table is read.
2. **Dump** the database anonymised *on the server*. With the default
   `stream` strategy a static shopware-cli is uploaded to a private temporary
   directory and rewrites personal columns inside the `SELECT`. The
   compressed stream goes directly into `<out>/dump.sql.zst`. With
   `--strategy shadow` the anonymised rows are copied into a temporary schema
   first and then dumped with `mysqldump` into `<out>/dump.sql.gz`. The dump
   runs in the background while steps 3–5 prepare the local side. A failure
   on one side stops the other.
3. **Code**: a local checkout (`--code`) or an rsync of the project without
   runtime data, uploads, backups and secrets (see [Code](#code)).
4. **docker compose up** of `dockware/shopware-essentials` with the PHP
   version of the server, plus Redis when the project uses it. The ports
   listen on `127.0.0.1` only (`--bind`).
5. **composer install** in the container (vendor/ is never copied) and, when
   the dump is ready, the database import in parallel. The import splits the
   dump in the container: all DDL first in one session, then the rows in
   128 MiB parts through up to 8 mysql sessions, largest first, views and
   triggers last. The InnoDB redo log is disabled during the import (MySQL
   8.0.21 and later; enabled again afterwards, also on failure). If the
   machine crashes in this window, the local database is lost: run the clone
   again. The local MySQL settings for the import and the indexers are stored
   in the database volume (`SET PERSIST`): buffer pool a quarter of the
   container memory up to 4 GiB, 2 GiB redo log,
   `innodb_flush_log_at_trx_commit=2`. The import sessions write no binlog.
6. **Verify** the anonymisation directly after the import (see
   [Verification](#verification)). When a check fails, or the run fails or
   is stopped after the import started, the clone removes the local data.
   When verification and composer install succeeded, the dump file is
   deleted (`--keep-dump` keeps it). When composer install failed, the dump
   stays for `--reuse-dump`.
7. **Prepare the shop**: domains are changed to `localhost` /
   `<slug>.localhost`, admin user `admin` / `shopware`, staging mode,
   `plugin:refresh`, `assets:install`, `theme:compile`, optional `post_sql`.
   Code synced from the server gets a new JWT key pair (`config/jwt` is never
   copied).
8. Optional headless frontend from the config, then **smoke tests**:
   storefront, Store API, Admin API login, frontend SSR.

At the end the command prints the shop URL, the admin URL, the domain
mapping, the Store API access keys, the compose command and the expiry date.

Local runtime settings: `APP_DEBUG=0` (`1` with `--app-env dev`) instead of
the value of the server, because the debug container checks every file on the
bind mount again. A `php-clone.ini` (fpm and cli) sets opcache to 512 MB /
100000 files with `revalidate_freq=2`, so code changes are visible within 2 s.
You can change both with `env:` or in `<out>/php-clone.ini`.

These items do not leave the server:

- personal data (anonymised before it is serialised),
- `.env` secrets (only the key names are read; secret values are set empty
  locally),
- `vendor/` (installed locally with composer),
- media (served from the source through an Apache redirect).

Connection keys are changed for the local stack: `DATABASE_URL`,
`MAILER_DSN`, `OPENSEARCH_URL` and the messenger DSNs get fixed local values.
`REDIS*` keys that end in `HOST` / `PORT` become `redis` / `6379`. `REDIS*`
keys that contain `URL` or `DSN` (`REDIS_URL`, `REDIS_DSN_CACHE`, …) become
`redis://redis:6379`. `REDIS*` keys that contain `PASS` are set empty. For
other secret-like keys with a URL value (`CART_DSN`, `SESSION_DSN`, …) only
the scheme leaves the server: a `redis`, `rediss`, `valkey` or `valkeys`
scheme points to the local Redis, all other schemes stay empty.

## Prerequisites

- Docker with Docker Compose v2 (`docker compose version`). The clone checks
  this before it connects to the server, also for `--dump-only` with the
  `stream` strategy, which takes shopware-cli from its image.
- `rsync` locally, when the code is synced from the server.
- ssh access without a prompt: `ssh <alias> true` must succeed (key in the
  agent or `--identity`). Unlock a 1Password or other ssh agent that asks for
  approval first. The host key of the server must be in
  `~/.ssh/known_hosts`.
- On the server: `rsync`, `php`, `mysql` / `mysqldump`, `gzip`. `zstd` is not
  necessary.
- Composer credentials if the project uses private packages or store
  plugins. Like composer, the clone merges all `auth.json` sources, per
  section (`http-basic`, `bearer`, `github-oauth`, …) and host. On a
  conflict the earlier source wins: `--composer-auth <file>`, the
  `COMPOSER_AUTH` variable, `auth.json` in the checkout,
  `$COMPOSER_HOME/auth.json`, `~/.composer/auth.json`,
  `~/.config/composer/auth.json`. The clone prints the sources it used.
- **Windows**: run the clone in WSL. Native Windows supports only `--code`
  (a local checkout) and `--dump-only`, because the code sync needs rsync with
  POSIX paths.
- **MariaDB** on the server: the local stack runs MySQL 8. The clone shows a
  warning. The import can fail or behave differently, for example with
  MariaDB-only collations such as `utf8mb4_uca1400_ai_ci`.

## Examples

```bash
# with a local checkout (fastest; the checkout is bind-mounted, nothing is written into it)
dockware-cli clone shop-stage --path /var/www/shop --code ~/projects/shop

# without a checkout: the code is synced from the server into <out>/code
dockware-cli clone shop-stage --path /var/www/shop

# recent orders only, search index rebuilt locally instead of transferred
dockware-cli clone shop-stage --path /var/www/shop --orders-since-days 90 --skip-search-index

# only the anonymised dump, no local environment
dockware-cli clone shop-stage --path /var/www/shop --dump-only

# keep the dump, then import it again later without a new dump
dockware-cli clone shop-stage --path /var/www/shop --keep-dump
dockware-cli clone shop-stage --path /var/www/shop --reuse-dump

# shadow strategy (needs CREATE DATABASE or an empty <db>_dwc_shadow schema)
dockware-cli clone ssh://deploy@shop.example.com:2222 --path /var/www/html --strategy shadow

# show and remove clones
dockware-cli clone list
dockware-cli clone purge --expired
```

## Flags

`<ssh-target>` is an ssh config alias, `user@host` or `ssh://user@host:port`.
A target that starts with `-` is rejected.

| Flag | Default | Meaning |
|---|---|---|
| `--path` | – (required) | Shopware project root on the server (holds `bin/console` and `composer.json`). |
| `--ssh-port` | ssh config | ssh port. |
| `--identity` | ssh config / agent | ssh private key. |
| `--ssh-option` | – | Extra `ssh -o` option, e.g. `ConnectTimeout=20` or `ProxyJump=bastion`. Repeatable. `StrictHostKeyChecking=no`/`off` and `UserKnownHostsFile=/dev/null` are rejected. |
| `--strategy` | `stream` | `stream`: anonymise inside the dump query, no writes on the server except a temporary shopware-cli. `shadow`: anonymised copy into a temporary schema, then dump. |
| `--shadow-db` | `<db>_dwc_shadow` | Shadow schema name. It must not exist or must be empty. |
| `--limit` | – | shopware-cli row limit `table:rows`. Repeatable. `stream` only; creates staging tables on the server. |
| `--orders-since-days` | config, else all | Keep only orders of the last N days with their child rows. `0` = all orders. |
| `--skip-search-index` | config, else false | Do not transfer `product_search_keyword` and `product_keyword_dictionary`; rebuild the search index locally. `=false` overrides a config `true`. |
| `--code` | – | Use this local checkout instead of the code sync. |
| `--exclude` | – | Extra rsync exclude pattern for the code sync. Repeatable; added to `exclude:` of the config. |
| `--max-code-size` | config, else 300 | Stop the code sync when the project is larger than this many MB. `-1` = no limit. |
| `--composer-auth` | – | `auth.json` for composer install; merged with the other sources (see Prerequisites). |
| `--php` | config, else server | PHP version of the local shop, e.g. `8.3`. |
| `--node` | config, else detected | Node major version of the shop container and the default frontend image, e.g. `22`. |
| `--config` | see [Configuration](#configuration-dockware-cloneyml) | Clone config file. |
| `--name` | previous name in `--out`, else `dwc-<host>-<hash>` | Compose project name. `<hash>` is the first 6 hex characters of the SHA-256 of `--path`, so two shops on one host get different names. Without `--name`, a new run in an existing `--out` uses the name of the previous run. When the name changes, the clone removes the old stack (containers and volumes) first. |
| `--out` | `./<name>` | Output directory. It must not be in the checkout, and the checkout must not be in it. An existing directory must be empty or hold a clone (`clone.log.json`). |
| `--force` | false | Use `--out` also when it holds a clone of another target or path, or other files without `clone.log.json`. |
| `--bind` | `127.0.0.1` | Host address for the published shop and frontend ports. Use `0.0.0.0` only if other machines must reach the clone. |
| `--port` | `8000` | First local HTTP port to try. |
| `--frontend-port` | `3000` | First local frontend port to try. |
| `--image` | `dockware/shopware-essentials:latest` | dockware image. |
| `--app-env` | `prod` | `APP_ENV` of the local shop. `dev` needs the dev dependencies in vendor/. |
| `--shopware-cli-version` | `0.19.7` | shopware-cli image tag for the `stream` dump. |
| `--verify-sample` | `50` | Rows per column that are compared against the server. |
| `--keep-unverified` | false | Keep the local data when anonymisation checks fail, or when the run fails or is stopped after the import started. For debugging only; never share such a clone. |
| `--dump-only` | false | Stop after the anonymised dump. The dump is not verified. |
| `--keep-dump` | false | Keep the dump file after a successful import and verification. |
| `--reuse-dump` | false | Import the kept dump of the previous run in `--out` instead of a new dump (see [Dump reuse](#dump-reuse)). Not with `--dump-only`. |
| `--retention-days` | config, else 30 | Days until the clone is expired (see [Clone registry](#clone-registry-list-and-purge)). |
| `--ssh-agent` | false | Mount the ssh agent of the host for composer install also when the project does not use ssh repositories. |
| `--no-frontend` | false | Do not start the configured headless frontend. |

Flags win over the config, also when they switch a setting off:
`--orders-since-days 0` exports all orders with `orders_since_days: 90` in the
config, `--skip-search-index=false` transfers the index with
`skip_search_index: true`, `--max-code-size -1` removes a configured limit.
`--orders-since-days` and `--retention-days` reject negative values.
`--max-code-size` accepts `-1` or a value above 0. `exclude:` and `--exclude`
are combined.

### ssh connection

All ssh and rsync calls of a run use one connection (OpenSSH
`ControlMaster`, socket in a private `/tmp/dwc-ssh-*` directory,
`ControlPersist=30m`, closed at the end). The connect timeout is 60 s for
slow tunnels and jump hosts. ssh runs with `BatchMode=yes`, so it never asks
for a password. `--ssh-option` values win over these defaults:
`--ssh-option ControlMaster=no` disables the shared connection. Windows
OpenSSH has no `ControlMaster`; there every call opens a new connection.

The clone does not let you switch off the host key check. If ssh reports an
unknown host key, connect once with `ssh <target>` or add the key with
`ssh-keyscan` and compare the fingerprint.

### Interrupts

Ctrl+C, `SIGTERM` and `SIGHUP` stop the run. The clone then removes the
temporary files on the server and stops the dump and import. When the import
had started, the clone also removes the local data (unless
`--keep-unverified`). A second Ctrl+C stops the process at once without this
cleanup.

## Configuration: `.dockware-clone.yml`

The clone uses the first config it finds:

1. `--config <file>`,
2. `<code>/.dockware-clone.yml` when `--code` is given,
3. `<path>/.dockware-clone.yml` on the server. The clone saves a copy as
   `<out>/.dockware-clone.remote.yml`. The file must be smaller than 1 MiB.

When a local config is used and the server also has one, the clone shows a
warning. Unknown keys are rejected. See
[`dockware-clone.example.yml`](dockware-clone.example.yml) for a commented
example.

| Key | Type | Meaning |
|---|---|---|
| `allow_tables` | list of globs | Extension tables whose rows are exported. All tables outside the Shopware core are exported without rows unless listed here. Built in: `swag_cms_extensions_*`, `swag_language_pack_*`, `swag_customized_products_template*`, `swag_social_shopping_sales_channel`. |
| `no_data` | list of globs | More tables to export without rows, also core tables. Built in: cart, queues, logs, sessions, tokens, documents, import/export files, admin UI settings (`user_config`), `*_log`, `*_logs`, `*_log_entry`. |
| `rules` | list of `{table, column, kind}` | Column rewrites on top of the defaults. A project rule for a column that has a default rule replaces it. A rule for a table or column that the server database does not have is an error. |
| `keep_columns` | list of `table.column` | Columns that a default rule clears (kind `null`, or an automatic `custom_fields` / JSON rule) and that you want to export unchanged. No globs. An entry for a column with another rule kind (e.g. `customer.email`) is an error; use `rules:` to change such a rule. |
| `expected_email_columns` | list of `table.column` globs | Columns where the verification accepts e-mail addresses, e.g. `acme_store.email` or `acme_*.contact_email`. |
| `retention_days` | int | Days until the clone is expired. `0` = default (30). |
| `env` | map | Overrides for the generated local environment (`shop.env`). Use it for keys that the clone set empty because they looked like credentials. Keys that the dockware image controls (`PHP_VERSION`, `NODE_VERSION`, `NVM_VERSION`, `TZ`, `XDEBUG_*`, …) are ignored with a warning; use `php:` / `node:`. |
| `post_sql` | path | SQL file that runs after the import, relative to the config file, e.g. sandbox payment keys. |
| `exclude` | list of rsync patterns | More paths that the code sync skips (see [Code](#code)). Ignored with `--code`. |
| `max_code_size_mb` | int | Stop the code sync when the project on the server is larger (decimal MB). Default 300, `-1` = no limit. |
| `orders_since_days` | int | Keep only orders of the last N days and their child rows. `0` = all orders. |
| `skip_search_index` | bool | Export `product_search_keyword` and `product_keyword_dictionary` without rows and rebuild them locally with `dal:refresh:index --only=product.indexer`. |
| `php` | `major.minor` | PHP version of the local shop. Default: the version of the server. |
| `node` | major | Node major version of the shop container and the frontend image. Default: detected (see [Versions](#versions)). |
| `frontend` | map | Headless frontend next to the shop, see [Frontend](#frontend). |

### Rules

```yaml
rules:
  - { table: acme_newsletter_lead, column: email, kind: email }
  - { table: acme_newsletter_lead, column: phone, kind: phone }
  - { table: order, column: customer_comment, kind: null }
```

Kinds: `email`, `first_name`, `last_name`, `company`, `street`, `zip`, `city`,
`phone`, `ip`, `username`, `token`, `password`, `empty_json_array`, `null`.
The values come from the row id, so the same row always gets the same
replacement. `NULL` and empty values stay as they are. `password` sets the
bcrypt hash of `shopware`, so all accounts (admin users, customers,
integrations) log in with that password. `null` clears the value (`NULL`,
`''` or `{}`, as the column allows). Generated e-mail addresses use the
domain `example.com`.

Default rules:

- names, e-mail, company, addresses, phone, IP addresses, birthdays, VAT ids,
  titles, comments and hashes of customers, customer addresses, order
  customers, order addresses and newsletter recipients;
- customer passwords, admin users, user access keys, integration secrets, app
  secrets;
- product review authors, e-mail addresses and comments;
- order comments and deep link codes, delivery tracking codes, transaction
  validation data, capture and refund references and reasons;
- promotion code payloads and `mail_template_type.template_data` (data of the
  last sent mail);
- **all** `custom_fields` and JSON columns of the customer and order tables
  (`customer`, `customer_*`, `order`, `order_*`, `newsletter_recipient`,
  `cart`, `product_review`, `mail_template_type`). Columns with prices,
  states and ids stay (e.g. `order.price`, `order_line_item.payload`,
  `order_transaction.amount`), because the administration needs them. JSON
  columns that a generated column of the same table reads are also not
  cleared, because the import would fail.

Default rules for columns that the server database does not have are
skipped, so one rule set works for several Shopware versions.

`system_config` rows are dropped when the key looks like a credential
(`password`, `secret`, `token`, `apikey`, `private`, `clientid`,
`merchantid`, `webhook`, `license`, `auth`, `dsn`, `username`, a segment that
ends in `key`, …) or when the JSON value holds a non-empty nested secret
(e.g. `"clientSecret": "…"`). `core.store.shopSecret` and `core.app.shopId`
are always dropped. The local shop uses the defaults; add sandbox keys with
`post_sql`.

### Frontend

```yaml
frontend:
  path: frontend            # directory in the code tree, mounted to /app
  image: node:22            # optional; default node:<major> from --node/node:, else the frontend dir, the code root or the server
  install: npm ci           # optional, runs before command
  command: npm run dev      # required
  port: 3000                # port in the container, published on --frontend-port
  env:
    NUXT_PUBLIC_SHOPWARE_ENDPOINT: ${SHOP_URL_INTERNAL}/store-api
    NUXT_PUBLIC_SHOPWARE_ACCESS_TOKEN: ${ACCESS_KEY:Headless}
    NUXT_PUBLIC_SITE_URL: ${FRONTEND_URL}
```

Placeholders in `env`: `${SHOP_URL}` (browser URL of the shop),
`${SHOP_URL_INTERNAL}` (`http://shop`, for server-side requests in the
compose network), `${FRONTEND_URL}` (`http://localhost:<frontend-port>`),
`${ACCESS_KEY}` (first storefront or headless sales channel) and
`${ACCESS_KEY:<sales channel name>}`. `node_modules` and `.nuxt` are in
volumes, so the checkout stays clean. `--no-frontend` skips the frontend.

## Database source

The clone reads `DATABASE_URL` like Symfony does: `.env.local.php` if it
exists, else `.env`, `.env.local` (not for `APP_ENV=test`), `.env.$APP_ENV`
and `.env.$APP_ENV.local`. The dump uses exactly this database. After the
import, the clone compares the sales channel ids with the server, so a dump
of another database is found.

Core tables are the tables of `vendor/shopware/core`, `storefront`,
`administration` and `elasticsearch`. All other tables, also those of
Shopware Commercial, are extension tables: the clone exports them without
rows unless they match `allow_tables`. The run prints these tables with
their approximate row counts and writes them to the audit.

## Verification

Directly after the import, before composer results are used and before the
shop is prepared, the clone runs these checks on the local database:

- the table count is the same as on the server;
- the sales channel ids are the same as on the server;
- each rewritten column holds only values with the generated pattern;
- sampled generated values (`--verify-sample` rows per column) do not exist
  on the server for the same row; the server receives only generated values
  and returns counts;
- tables exported without rows are empty;
- no credentials in `system_config` keys or values, checked with SQL and
  again in Go with the same patterns;
- no e-mail addresses outside `example.com` in any exported text column,
  except in expected columns. Built-in expected columns:
  `system_config.configuration_value`, `mail_template*.*`,
  `mail_header_footer*.*`, `sales_channel*.*`, `cms_*.*`,
  `category_translation.slot_config`, `landing_page_translation.slot_config`,
  `product*.*`, `snippet.*`, `app*.*`, `flow_sequence.config` (recipients of
  flow mails). They never apply to the customer and order tables;
  there only `expected_email_columns` counts.

When a check fails, the clone **fails closed**: it removes the containers,
the database volume and the dump, writes `wiped: true` to `clone.log.json`
and exits with an error. The same happens when the run fails or is stopped
at any point after the import started. The output directory with the code
and the audit stays; `clone list` shows the clone as `wiped`. Add `rules:`
for the listed columns, or `expected_email_columns:` for shop addresses, then
clone again. `--keep-unverified` keeps the local data for debugging; the run
still exits with an error. Never share an unverified clone.

`--dump-only` stops before the import, so the dump is **not** verified. The
clone shows a warning. Do not share such a dump.

## Dump reuse

The dump file is deleted after the import when verification and composer
install succeeded. `--keep-dump` keeps it in `<out>`. When composer install
fails, the dump stays. When a run stops before its dump starts, the dump of
the run before stays usable. `--reuse-dump` imports this dump again instead
of a new dump from the server. It works only when all of these are true:

- the previous run in the same `--out` left a dump (`--keep-dump`,
  `--dump-only`, or composer install failed),
- target and `--path` are the same,
- the anonymisation plan (rules hash: rules, tables without rows,
  `orders_since_days`) is the same. The order window counts by its number of
  days, so a dump stays reusable on the next day; the clone uses the order
  cutoff date of the original dump,
- the dump file has the same size as recorded in `clone.log.json`.

Otherwise the run stops with a `config` error that gives the reason.

## Excluding

### Code

Built-in rsync excludes. A leading `/` anchors the pattern at the project
root. The anchored data directories have no trailing `/`, so they also match
symlinks (release-based deploys link them to a shared directory).

- secrets, at any depth: `.env`, `.env.*` (also `.env.dist` and
  `.env.example`), `auth.json`, `.npmrc`, `id_rsa*`, `id_ed25519*`,
  `id_ecdsa*`, `*.pem`, `*.key`, `*.p12`, `*.pfx`
- secrets and state at the root: `/install.lock`, `/config/jwt`,
  `/config/secrets`, `/.shopware-project.yml`, `/.shopware-project.yaml`
- tooling: `.git/`, `.idea/`, `.DS_Store`, `node_modules/`, `.nuxt/`,
  `.output/`
- installed or generated: `/vendor`, `/var`, `/public/media`,
  `/public/thumbnail`, `/public/sitemap`, `/public/theme`,
  `/public/bundles`, `/public/build`
- data and backups: `/files`, `/uploads`, `/import`, `/export`,
  `/backup*/`, `/dump*/`
- dumps, archives and logs: `*.sql`, `*.sql.zst`, `*.sql.bz2`, `*.dump`,
  `*.gz`, `*.xz`, `*.zip`, `*.7z`, `*.rar`, `*.tar`, `*.tar.*`, `*.tgz`,
  `*.bak`, `*.log`
- `*.csv`, `*.xlsx`, `*.xls`, except in `Resources/`, `tests/` and `Tests/`
  directories below `/custom/` and `/src/` (plugin fixtures)

Symlinks that point outside the project (for example to a Deployer
`shared/` directory) are copied as their content.

Files larger than 50 MiB are skipped. Before the transfer a dry run measures
the project. When it is larger than `max_code_size_mb` (default 300), the
clone stops and shows the largest directories, so you can exclude them. The
dry run also lists the skipped large files. With `-1` (no limit) there is no
dry run, and skipped files are not listed.

`exclude:` and `--exclude` take rsync patterns, one per entry:

```yaml
exclude:
  - /public/catalogs/          # a directory below the project root
  - /custom/static-plugins/Legacy*/
  - "*.psd"                    # a file pattern at any depth
  - "**/fixtures/large/"       # a directory at any depth
```

A trailing `/` matches only directories, `*` does not cross `/`, `**` does.
Excludes apply only to the code sync from the server. With `--code` the
checkout is used as it is.

### Tables

- `no_data:` adds tables without rows (globs, core tables allowed).
- Extension tables have no rows unless they match `allow_tables:`. Allow only
  tables without personal data. If an allowed table has text columns without
  a rule, the clone shows a warning.
- `orders_since_days` / `--orders-since-days N` keeps only orders whose
  `order_date_time` is in the last N days, with their child rows (line items,
  deliveries, transactions, addresses, order customers, …). The cutoff is
  calculated once (UTC, per day) and used for all tables. If the server has
  no `order.order_date_time` column, the clone shows a warning and exports all
  orders.
- `skip_search_index` / `--skip-search-index` does not transfer the product
  search index; the clone rebuilds it after the import. This is useful only
  for small catalogs: the indexer can take hours on a large catalog, while
  the import of the rows takes seconds. Derived data such as
  `cheapest_price*` or `product_category_tree` is needed by the storefront and
  only the indexer rebuilds it, so do not exclude it.

`--limit table:rows` (`stream` only) limits the rows per table with
shopware-cli. It creates staging tables on the server.

## Local environment

- **Ports** are published on `127.0.0.1` (`--bind`), so other machines in
  the network cannot reach the clone.
- **Compose labels**: all services get `dockware.clone.source`
  (`<target>:<path>`), `dockware.clone.created` and
  `dockware.clone.expires`. Volumes get only `dockware.clone.source`.
- **ssh agent**: the ssh agent of the host is mounted into the shop container
  only when composer needs ssh keys (a `repositories` URL in `composer.json`
  or a `source`/`dist` URL in `composer.lock` uses ssh) or with
  `--ssh-agent`. After composer install and import, the clone recreates the
  shop container without the agent. The database volume stays. The agent is
  also removed when the run fails or is stopped. On macOS the clone uses the
  agent socket of Docker Desktop.
- The dockware image contains an ssh key pair that is the same in every
  pull. The clone deletes `/var/www/.ssh/id_rsa` in the container. Never
  authorise this key on a server.
- **Linux**: a bind mount keeps the owner of the host. The clone makes
  `files`, `public`, `config/jwt` and `custom/plugins` writable for
  `www-data` (uid 33): synced code gets `chmod a+rwX` (also `var`), a
  checkout gets an ACL with `setfacl`. If that fails, the clone shows the
  host command to run.
- The output directory is created with mode `0700`. The clone does not use
  an output directory that holds a clone of another target or path, or
  other files without `clone.log.json`, unless you give `--force`.

## Versions

- **PHP**: `major.minor` of the server; change it with `--php` or `php:`. The
  dockware image changes PHP at start (`PHP_VERSION`).
- **PHP extensions**: the clone installs each extension that the server has
  (`php -m`) and the container does not have from the apt sources of the
  image (`php<ver>-<ext>`, e.g. `php8.3-imagick`) and restarts php-fpm before
  composer runs. Profilers and debuggers (tideways, xdebug, pcov, blackfire,
  newrelic, opcache, ddtrace, excimer) are skipped. An extension without a
  package stays missing with a warning.
- **Node** for the shop container: `--node`, else `node:` in the config, else
  the first of `.nvmrc`, `.node-version`, `package.json` (`volta.node`,
  `engines.node`) in the code root, else `node -v` on the server. A missing
  version is installed with nvm and set as default.
- **Frontend image**: `frontend.image`, else `node:<major>` from `--node` /
  `node:`, else detected from the frontend directory, the code root or the
  server, else `node:lts`.

The node version and the added PHP extensions are in the shop container, not
in a volume. They stay after `docker compose stop` / `start`. After
`docker compose down` and `up` the image default node is active again and the
extensions are gone. Run the clone again, or install them manually:
`docker compose -f <out>/docker-compose.yml exec shop bash -lc 'nvm install <major> && nvm alias default <major>'`
and `docker compose -f <out>/docker-compose.yml exec -u root shop bash -c 'apt-get update && apt-get install -y php8.3-<ext> && service php8.3-fpm restart'`.

The clone prints the result and the source of each value, e.g.
`PHP 8.3 (server), Node 22 (checkout)`.

## Media

Media and thumbnails are not copied. Apache redirects requests for files that
do not exist locally to the public media URL of the source (from
`config/packages/*.yaml` `filesystem.public.url`, else `APP_URL`). Local
uploads and an existing `public/media` in a checkout are used first. Without
a usable URL the clone shows a warning and missing media return 404.

## Clone registry, list and purge

Each clone is registered with an expiry date in an append-only file:

- macOS: `~/Library/Application Support/dockware-cli/clones.jsonl`
- Linux: `$XDG_CONFIG_HOME/dockware-cli/clones.jsonl` (default
  `~/.config/dockware-cli/clones.jsonl`)
- Windows: `%AppData%\dockware-cli\clones.jsonl`
- or the file in `DOCKWARE_CLONE_REGISTRY`.

A run is registered when local data exists (a dump or a stack), also when
the run fails. The expiry date is the start date plus `--retention-days`,
else `retention_days` of the config, else 30 days. Expired clones are not
removed automatically. Each clone run shows a warning for expired clones that
still exist.

`dockware-cli clone list` shows name, source (`<target>:<path>`), creation
date, expiry date, verification state (`yes`, `no`, or `wiped` when a failed
verification removed the local data) and output directory. `EXPIRED` marks
expired clones, `missing` marks clones whose output directory does not exist.
Purged clones are not shown.

`dockware-cli clone purge` removes clones: `docker compose down -v`
(containers and the cloned database) and the files that the clone wrote into
its output directory: `clone.log.json`, `docker-compose.yml`, `shop.env`,
`frontend.env`, `apache-*.conf`, `php-clone.ini`,
`.dockware-clone.remote.yml`, `dump.sql*`, and `code/` when the clone synced
the code. When nothing else is in the directory, purge removes it. Otherwise
purge keeps the directory and lists the other entries (`kept …`).

| Flag / argument | Meaning |
|---|---|
| `name...` | Clones to remove, by name or output directory (see `clone list`), also wiped clones. An output directory that is not in the registry (e.g. from an older version) is accepted when it holds `clone.log.json` and `docker-compose.yml`. |
| `--expired` | Remove all expired clones. Can be combined with names. |
| `--yes` | Do not ask for confirmation. Necessary without an interactive terminal. |

Purge does not remove relative paths, `/`, the home directory or its parents,
or an existing directory without `clone.log.json`. If the output directory
does not exist anymore, purge removes only the registry entry.

The checkout given with `--code` is never changed.

## Errors

Each failure is shown as `Error: [<step>] <message>`, followed by the end of
the output of the failed tool and a `hint:` line. The hint is specific to the
step, so a `Connection refused` from composer is not explained as an ssh
problem. The exit code is non-zero, also when the shop runs but
verification or smoke checks failed. The audit file `<out>/clone.log.json`
holds the same information in `error`, `error_step` and `error_hint`, with
phases, warnings and all checks.

```
Error: [ssh] ssh to deploy@shop.example.com failed: exit status 255
  output:
    deploy@shop.example.com: Permission denied (publickey,password).
  hint: the server rejected the ssh login: check user and host (ssh config alias), pass --identity <key>, or authorise your public key on the server
```

```
Error: [shadow] the database user shop@% may not create the shadow schema shop_dwc_shadow
  hint: use --strategy stream (default, needs no server writes) or ask the hoster for CREATE on that schema
```

Warnings (`WARN …`) do not stop the clone. They are also in the audit.

| Step | Typical cause | What to do |
|---|---|---|
| `config` | unknown config key, bad rule kind, rule for a missing table or column, invalid `keep_columns` entry, output directory in the checkout or checkout in the output directory, output directory with a clone of another target or with other files, invalid `--php`/`--node`/`--exclude`/`--ssh-option`, negative `--orders-since-days`/`--retention-days`, `--max-code-size` below `-1` or `0`, dump not reusable, unreadable `--composer-auth`, code sync on native Windows | fix the config or flag; the message gives the key |
| `ssh` | agent refused / `signing failed` (1Password locked), `Permission denied (publickey)`, host key unknown or changed, timeout, unknown host, connection lost | unlock the agent (`ssh-add -l`), `--identity <key>`, connect once with `ssh <target>`, check VPN, firewall, `--ssh-port` |
| `probe` | wrong `--path`, no Shopware project there (`vendor/shopware/core` missing), files not readable, `php`/`mysql` missing on the server | check `--path`, permissions and the tools on the server |
| `database` | `DATABASE_URL` not readable, `Access denied`, MySQL not reachable from the ssh host | fix `DATABASE_URL` or the `.env` permissions on the server |
| `shadow` | the database user may not `CREATE DATABASE`, `binlog_format=STATEMENT`, shadow schema exists and is not empty | `--strategy stream`, or ask the hoster for an empty `<db>_dwc_shadow` |
| `upload` | `/tmp` not writable or mounted `noexec`, binary does not run on the server | `--strategy shadow` |
| `dump` | shopware-cli or mysqldump failed, disk full, MySQL error 3699 (a large `system_config` value exceeded `regexp_time_limit` in the secret filter) | read the output; free `/tmp` on the server; for 3699 raise `regexp_time_limit` on the server (global, ask the hoster) or add `system_config` to `no_data` and add needed keys with `post_sql` |
| `rsync` | rsync missing locally or on the server, files not readable (exit 23), project larger than `max_code_size_mb` | install rsync, fix permissions or add `exclude:`, raise `--max-code-size`, `--code <checkout>` |
| `docker` | daemon not running, Docker Compose v2 missing, port in use, image pull failed, container not healthy, nvm cannot download the `--node` version | start Docker, install the compose plugin, `--port` / `--frontend-port`, check `--image`, network or proxy |
| `composer` | `Authentication required`, private git package without ssh key, lock file needs another PHP version or extension, packagist not reachable | `--composer-auth` / `auth.json` / `COMPOSER_AUTH`; load the key into the agent; `--php`; check network or proxy; if the import had finished, the dump stays: run again with `--reuse-dump` |
| `import` | SQL error while loading the dump (MySQL/MariaDB difference, disk full), local MySQL not running | read the SQL error; `docker compose -f <out>/docker-compose.yml logs shop`; the local data was removed |
| `verify` | a column still holds server values (missing rule), unexpected e-mail addresses, secrets in `system_config`, other database dumped | add a `rules:` or `expected_email_columns:` entry; the local data was removed |
| `post-import` | `bin/console` step failed (plugin refresh, theme compile, search index) | run the printed command in the container |
| `frontend` | frontend container failed, placeholder for a missing sales channel | check `frontend:` and `docker compose logs frontend` |
| `smoke` | storefront, Store API, Admin API or frontend SSR does not return 200 | `docker compose -f <out>/docker-compose.yml logs shop` |
