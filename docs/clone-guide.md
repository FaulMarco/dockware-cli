# dockware-cli clone – guide

This guide shows how to copy a Shopware shop from a server into a local
dockware environment with anonymised data. Full reference:
[`clone.md`](clone.md).

## What it does

1. Reads the server over ssh: Shopware, PHP, MySQL and Node versions, PHP
   extensions, schema, domains, `.env` key names.
2. Dumps and anonymises the database **on the server** (default `stream`: no
   writes except a temporary shopware-cli in `/tmp`). Personal data does not
   leave the server.
3. Takes the code from a local checkout (`--code`) or syncs it from the
   server with rsync, without `vendor/`, uploads, backups and secrets.
4. Starts `dockware/shopware-essentials` with the PHP version of the server
   and runs `composer install` in the container. The dump runs at the same
   time. The ports listen on `127.0.0.1` only.
5. Verifies the anonymisation. **When a check fails, the clone removes the
   containers, the database and the dump.**
6. Changes the domains to `localhost`, sets the admin login `admin` /
   `shopware`, compiles the theme and runs smoke tests.
7. Does not copy media. Media requests are redirected to the source.
8. Registers the clone with an expiry date (default 30 days).

## Prerequisites

- Docker with Docker Compose v2 (`docker compose version`).
- `rsync` locally, if you do not use `--code`.
- ssh without a prompt: `ssh <alias> true` succeeds (unlock 1Password
  first). The host key of the server is in `~/.ssh/known_hosts`.
- On the server: `rsync`, `php`, `mysql`, `gzip`.
- Composer credentials for private packages or the Shopware Store:
  `auth.json` in the checkout, `~/.composer/`, `~/.config/composer/`, or
  `--composer-auth <file>`. The clone merges all sources like composer does.
- Windows: use WSL. Native Windows works only with `--code` or
  `--dump-only`.
- MariaDB on the server: the local stack uses MySQL 8. The clone shows a
  warning; the import can fail.

## Build the binary

You do not need a local Go installation:

```bash
cd ~/dockware-cli
docker run --rm -v "$PWD":/src -v dwc-gocache:/root/.cache -v dwc-gomod:/go/pkg/mod \
  -w /src golang:1.19 sh -c 'GOOS=darwin GOARCH=arm64 go build -o build/dockware-cli .'
alias dockware-cli=~/dockware-cli/build/dockware-cli
```

Set `GOOS` / `GOARCH` for your system (e.g. `linux` / `amd64`).

## New project

1. **Target and path.** Target: an alias in `~/.ssh/config`, `user@host` or
   `ssh://user@host:port`. `--path`: project root on the server (holds
   `bin/console` and `composer.json`).

   ```bash
   ssh shop-stage 'ls /var/www/shop/bin/console /var/www/shop/composer.json'
   ```

2. **Use stage, not live.** The clone only reads the database, but it uploads
   shopware-cli to `/tmp` and the dump adds load.
3. **Config** (optional). Template:
   [`dockware-clone.example.yml`](dockware-clone.example.yml). The clone
   uses the first of: `--config <file>`, `.dockware-clone.yml` in the
   checkout (with `--code`), `.dockware-clone.yml` in `--path` on the server.
   A config on the server lets the whole team use the same rules.
4. **Run** from a work directory outside the checkout. The clone creates
   `./<name>/` with the compose file and `clone.log.json`. The default name
   is `dwc-<host>-<hash of the path>`.

   ```bash
   mkdir -p ~/clones && cd ~/clones
   dockware-cli clone shop-stage \
     --path /var/www/shop \
     --code ~/projects/shop \
     --port 8300
   ```

   Without `--code` the code is synced from the server (stops above 300 MB).
5. **Result:** shop URL, admin URL, domain mapping, Store API access keys,
   compose command and expiry date. Exit code 0 means that import,
   anonymisation check and smoke tests passed.

## Anonymisation

The default rules anonymise customers, addresses, orders, newsletter
recipients, reviews, admin users and secrets. They also clear all
`custom_fields` and JSON columns of the customer and order tables.

| Task | Config |
|---|---|
| Anonymise more columns | `rules: [{table, column, kind}]` |
| Export a cleared column unchanged | `keep_columns: [order.custom_fields]` |
| Accept e-mail addresses in a column | `expected_email_columns: [acme_store.email]` |
| Export rows of an extension table | `allow_tables: [acme_*]` |

- Extension tables (all tables outside `vendor/shopware/core`, `storefront`,
  `administration`, `elasticsearch`, also Shopware Commercial) have no rows
  unless listed in `allow_tables`. The run lists them with approximate row
  counts. Allow only tables without personal data.
- A rule for a table or column that does not exist is an error.
- The clone checks the anonymisation directly after the import. If a check
  fails, or the run fails or is stopped after the import started, the clone
  removes the local data (`clone list` shows `wiped`). Read the failed
  checks, add rules, and run the clone again. `--keep-unverified` keeps the
  data for debugging. Never share it.
- `--dump-only` does not verify the dump. Do not share it.

## Excluding

| What | Config | Flag |
|---|---|---|
| Files or directories in the code sync (without `--code`) | `exclude:` (rsync patterns) | `--exclude '/custom/static-plugins/Old/'` (repeatable) |
| Code sync size limit | `max_code_size_mb: 500` | `--max-code-size 500`, `-1` = no limit |
| Tables without rows (also core) | `no_data: [table, prefix_*]` | – |
| Orders of the last N days only | `orders_since_days: 90` | `--orders-since-days 90`, `0` = all |
| Rebuild search index locally | `skip_search_index: true` | `--skip-search-index` |

- Flags win over the config, also when they switch a setting off
  (`--orders-since-days 0`, `--skip-search-index=false`).
- Secrets (`.env*`, `auth.json`, `.npmrc`, private keys) are excluded at all
  depths. Full list: [`clone.md` → Code](clone.md#code).

## Versions

- **PHP:** `--php` → `php:` → server.
- **PHP extensions:** the clone installs missing extensions from the
  `php -m` list of the server with apt. Profilers and debuggers (tideways,
  xdebug, pcov) are skipped.
- **Node:** `--node` → `node:` → `.nvmrc` / `.node-version` / `package.json`
  → server. The clone installs missing versions with nvm.
- **Frontend image** (headless): `frontend.image` → `--node` / `node:` →
  detected → server → `node:lts`.
- `docker compose down` + `up` resets Node and removes the added extensions.
  Run the clone again.

## Repeat a clone

- The dump is deleted after a verified import with a successful composer
  install. `--keep-dump` keeps it. When composer install fails after the
  import, the dump stays.
- `--reuse-dump` imports the kept dump in the same `--out` again, without a
  new dump from the server. Target, path and rules must be the same. The
  dump stays usable on the next day with the same `orders_since_days`.
- A new run in the same `--out` uses the name of the previous run. With a
  new `--name`, the clone removes the old stack first.
- The clone does not use an output directory that holds a clone of another
  shop or other files. Use `--out`, `--name` or `--force`. The checkout and
  `--out` must not be inside each other.

## Security

- The ports listen on `127.0.0.1` only. `--bind 0.0.0.0` opens them to the
  network.
- The ssh agent is mounted only when composer needs ssh repositories (or
  with `--ssh-agent`). The clone removes it after composer install, and also
  when the run fails or is stopped.
- You cannot switch off the host key check with `--ssh-option`.
- Remove clones that you do not need anymore (see [Clean up](#clean-up)).

## Errors

Each error gives the step, the tool output and a hint. The exit code is
non-zero, and `<name>/clone.log.json` holds `error_step` and `error_hint`.

```
Error: [ssh] ssh to deploy@shop.example.com failed: exit status 255
  output:
    deploy@shop.example.com: Permission denied (publickey,password).
  hint: the server rejected the ssh login: check user and host (ssh config alias), pass --identity <key>, or authorise your public key on the server
```

| Step | Common cause | Fix |
|---|---|---|
| `config` | unknown key, invalid flag value, rule for a missing column | the message gives the key or flag |
| `ssh` | key rejected, agent locked, unknown host key, timeout | `ssh <alias> true`, `--identity`, VPN |
| `probe` | wrong `--path`, not Shopware, no read access | check the path |
| `database` | database credentials from the server `.env` rejected | `DATABASE_URL` on the server |
| `shadow` | the database user cannot create a schema | default `--strategy stream` |
| `upload` / `dump` | `/tmp` full or `noexec` | free `/tmp`, `--strategy shadow` |
| `rsync` | rsync missing, no read access (exit 23), project too large | install rsync, `exclude:`, `--max-code-size`, `--code` |
| `docker` | Docker not running, Compose v2 missing, port in use, image pull | start Docker, `--port`, network or proxy |
| `composer` | missing credentials, PHP version, no network | `auth.json` / `--composer-auth`, `--php`; after the import, run again with `--reuse-dump` |
| `verify` | column with server values, unexpected e-mail addresses, secrets in `system_config` | `rules:` or `expected_email_columns:`, run again |
| `import` / `post-import` / `smoke` | SQL error, console command failed, page not 200 | `docker compose -f <name>/docker-compose.yml logs shop` |

Full table: [`clone.md` → Errors](clone.md#errors).

## Clean up

```bash
dockware-cli clone list               # all clones with expiry date
dockware-cli clone purge <name>       # remove one clone
dockware-cli clone purge --expired    # remove all expired clones
```

Purge removes the containers, the database volume and the files the clone
wrote into the output directory (also `code/` when it was synced). The
directory goes when nothing else is in it. Purge also takes wiped clones and
output directories of older versions that are not in the registry. A
`--code` checkout is never changed. Each clone run shows a warning when
expired clones still exist.
