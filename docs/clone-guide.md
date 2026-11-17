# dockware clone – guide

Set up a Shopware shop from a server as a local dockware environment with
anonymised data. Full reference: [`clone.md`](clone.md).

## What it does

1. Reads the server over ssh: Shopware, PHP, MySQL and Node versions, PHP
   extensions, schema, domains, `.env` key names.
2. Dumps and anonymises the database **on the server** (default `stream`: no
   writes except a temporary shopware-cli in `/tmp`). Personal data never
   leaves the server in plain text.
3. Takes the code from a local checkout (`--code`) or rsyncs it from the
   server, without `vendor/`, uploads, backups and secrets.
4. Starts `dockware/shopware-essentials` with the server's PHP version and runs
   `composer install` in the container. The dump runs in parallel; the import
   uses up to 8 MySQL connections.
5. Checks the anonymisation, rewrites domains to `localhost`, sets admin
   `admin` / `shopware`, compiles the theme, runs smoke tests.
6. Media are not copied; they are redirected to the source.
7. Runs with `APP_DEBUG=0` (`--app-env dev`: `1`) and a larger opcache
   (`<name>/php-clone.ini`): 30–55 % faster pages on a large shop on macOS.

## Prerequisites

- Docker is running (`docker info`).
- Non-interactive ssh: `ssh <alias> true` succeeds (unlock 1Password first).
- On the server: `rsync`, `php`, `mysql`, `gzip`.
- Composer credentials for private packages / Shopware Store: `auth.json` in
  the checkout, `~/.composer/`, `~/.config/composer/`, or
  `--composer-auth <file>`. All sources are merged like composer does.

## Build the binary

No local Go needed:

```bash
cd ~/dockware-cli
docker run --rm -v "$PWD":/src -v dwc-gocache:/root/.cache -v dwc-gomod:/go/pkg/mod \
  -w /src golang:1.22 sh -c 'GOOS=darwin GOARCH=arm64 go build -o build/dockware-cli .'
alias dockware-cli=~/dockware-cli/build/dockware-cli
```

## New project

1. **Target and path.** Target: `~/.ssh/config` alias, `user@host` or
   `ssh://user@host:port`. `--path`: project root on the server (holds
   `bin/console` and `composer.json`).

   ```bash
   ssh shop-stage 'ls /var/www/shop/bin/console /var/www/shop/composer.json'
   ```

2. **Use stage, not live.** The clone is read-only, but it uploads
   shopware-cli to `/tmp` and the dump adds load.
3. **Config** (optional): keep it outside the checkout, e.g.
   `~/clones/<project>-clone.yml`. Template:
   [`dockware-clone.example.yml`](dockware-clone.example.yml). A
   `.dockware-clone.yml` in the checkout is found automatically with `--code`.
4. **Run** from a work folder outside the checkout; it creates `./<name>/`
   with the compose file, dump and `clone.log.json`:

   ```bash
   mkdir -p ~/clones && cd ~/clones
   dockware-cli clone shop-stage \
     --path /var/www/shop \
     --code ~/projects/shop \
     --config ~/clones/shop-clone.yml \
     --name dwc-shop --port 8300
   ```

   Without `--code` the code is synced from the server (aborts above 300 MB).
5. **Result:** shop URL, admin URL, domain mapping, Store API access keys and
   the compose command. Exit code 0 means import, anonymisation check and
   smoke tests passed.

## Excluding

| What | Config | Flag |
|---|---|---|
| Files/folders in code sync (without `--code`) | `exclude:` (rsync patterns) | `--exclude '/custom/static-plugins/Old/'` (repeatable) |
| Code sync size limit | `max_code_size_mb: 500` | `--max-code-size 500`, `-1` = no limit |
| Tables without rows (core too) | `no_data: [table, prefix_*]` | – |
| Extension tables **with** rows | `allow_tables: [table]` | – |
| Orders of the last N days only | `orders_since_days: 90` | `--orders-since-days 90`, `0` = all |
| Rebuild search index locally | `skip_search_index: true` | `--skip-search-index` |
| Anonymise extra columns | `rules:` | – |

- Extension tables come without rows unless listed in `allow_tables`; the run
  lists them with approximate row counts. Allow only tables without personal
  data.
- Flags override the config, also when disabling (`--orders-since-days 0`,
  `--skip-search-index=false`).
- Built-in excludes: [`clone.md` → Code](clone.md#code).

## Versions

- **PHP:** `--php` → `php:` → server.
- **PHP extensions:** missing ones from the server's `php -m` are installed via
  apt; profilers/debuggers (tideways, xdebug, pcov) are skipped.
- **Node:** `--node` → `node:` → `.nvmrc` / `.node-version` / `package.json` →
  server. Missing versions are installed via nvm.
- **Frontend image** (headless): `frontend.image` → `--node` / `node:` →
  detected → server → `node:lts`.
- `docker compose down` + `up` resets Node and drops the added extensions;
  rerun the clone.

## Errors

Each error names the step, the tool output and a hint; the exit code is
non-zero and `<name>/clone.log.json` holds `error_step` and `error_hint`.

```
Error: [ssh] ssh to dockware@127.0.0.1 failed: exit status 255
  output:
    dockware@127.0.0.1: Permission denied (publickey,password).
  hint: the server rejected the ssh login: check user and host (ssh config alias), pass --identity <key>, or authorise your public key on the server
```

| Step | Common cause | Fix |
|---|---|---|
| `config` | unknown key, invalid flag value | message names key/flag |
| `ssh` | key rejected, agent locked, unknown host, timeout | `ssh <alias> true`, `--identity`, VPN |
| `probe` | wrong `--path`, not Shopware, no read access | check path |
| `database` | DB credentials from server `.env` rejected | `DATABASE_URL` on the server |
| `shadow` | DB user cannot create a schema | default `--strategy stream` |
| `upload` / `dump` | `/tmp` full or `noexec` | free `/tmp`, `--strategy shadow` |
| `rsync` | read access (exit 23), project too large | `exclude:`, `--max-code-size`, `--code` |
| `docker` | Docker down, port in use, image pull | start Docker, `--port`, network/proxy |
| `composer` | missing credentials, PHP version, no network | `auth.json` / `--composer-auth`, `--php` |
| `import` / `verify` / `post-import` / `smoke` | SQL error, unchecked column, page not 200 | `docker compose -f <name>/docker-compose.yml logs shop` |

Full table: [`clone.md` → Errors](clone.md#errors).

## Clean up

```bash
docker compose -f ~/clones/<name>/docker-compose.yml --profile frontend down -v
rm -rf ~/clones/<name>
```

A `--code` checkout is never modified.

## Example: large shop (stage)

Shopware 6.6.10, PHP 8.3, Node 18, MySQL 8.4, ~36 GB database (~12 GB
analytics/extension tables, cloned without rows), ~166,000 products. The
config excludes telemetry and thumbnails and transfers derived data (prices,
category tree, search index): rebuilding it locally took over 2 hours. A
project plugin's product tables had to go into `allow_tables`, otherwise every
product page returned 400 (`CartException: Payload key ... not found`). Run
time: ~8 min including the dump.
