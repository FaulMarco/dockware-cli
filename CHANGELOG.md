# Changes in PHPUnuhi

All notable changes of PHPUnuhi releases are documented in this file
using the [Keep a CHANGELOG](https://keepachangelog.com/) principles.

## [Unreleased]

### Added

- `dockware-cli clone`: copy a Shopware 6 shop over SSH into a local dockware environment. See docs/clone.md.
- The database is anonymised on the server before the transfer (`stream` or `shadow` strategy).
- Default anonymisation rules for customers, orders, newsletter recipients, reviews, admin users and secrets, plus all custom fields and JSON columns of customer and order tables.
- `.dockware-clone.yml` with `rules`, `allow_tables`, `no_data`, `keep_columns`, `expected_email_columns`, `env`, `post_sql`, `exclude`, `retention_days` and more; also read from the project root on the server.
- Anonymisation check directly after the import; a failed check, error or stop after the import removes the local data (`--keep-unverified` keeps it).
- Code from a local checkout (`--code`) or synced from the server without secrets, uploads, backups and archives; size limit (`--max-code-size`) and extra excludes (`--exclude`).
- vendor/ installed with composer in the container; PHP version, PHP extensions and Node version taken from the server or the project.
- Smaller dumps: orders of the last N days only (`--orders-since-days`), search index rebuilt locally (`--skip-search-index`).
- Media served from the source instead of copied.
- Ports bound to 127.0.0.1 (`--bind`); ssh agent mounted only while composer needs it.
- Dump deleted after a verified import (`--keep-dump` keeps it); import of a kept dump (`--reuse-dump`), also on the next day.
- Clone registry with expiry date (`--retention-days`), `dockware-cli clone list` and `dockware-cli clone purge`; purge removes only the files a clone wrote.
- Each error names its step and gives a hint; the audit is written to `clone.log.json`.

## [0.7]

### Fixed

- Fixed building problem on Windows. Removed the interactive-terminal check.

## [0.6]

### Added

- Initial version

