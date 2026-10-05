# NoBackups

> **Tired of your team forgetting the most important rule in ops?**
> No, not "never deploy on a Friday". The *other* one: **3-2-1**. Three copies of your data, on two kinds of storage, with one offsite.

NoBackups is named after the situation it gets you out of. It's one small static binary you drop on a Linux server. You tell it what matters and where to send it, and it handles the rest on a schedule. That way "so… does anyone have a backup?" stays a hypothetical, not something posted in the team chat at 3 AM!

**What's in the box**

- **Any S3-compatible storage**: Alarik, Hetzner Object Storage, RustFS, MinIO, Garage, AWS S3, Backblaze B2, Cloudflare R2, … plus local directories (second disk, NFS). Your data, your bucket list.
- **Multiple destinations per job**: the archive is built once and streamed to every destination in parallel. If one destination fails, the others still finish, so 3-2-1 takes one line of config.
- **Streaming**: nothing is staged on local disk. Memory use is about `part_size_mb` per S3 destination. Light enough for even the most *byte*-sized VPS.
- **zstd / gzip compression** and **age encryption** (passphrase or public keys). Squeezed, sealed, delivered.
- **Retention**: keep the last N snapshots and/or everything newer than N days. Old snapshots are let go gracefully.
- **Hooks**: run commands before and after a job (database dumps, stopping containers, cleanup)
- **Cron schedules** with a systemd-friendly daemon, plus manual runs for the "wait, before I touch prod…" moments
- **Restore**: full, or limited to specific paths. The part people forget to test.
- **Discord notifications**: colour-coded embeds with status, host, size and per-destination results. Set them globally or per job.
- **Webhooks**: generic JSON notifications for Slack, Mattermost, ntfy, healthchecks.io, …
- **Locking**: the daemon and a manual run never back up the same job at the same time. No double-booking.

📚 **Full documentation** lives in [`docs/`](docs/) (a [Mintlify](https://mintlify.com) site: run `make docs` to preview it locally).

Hungry for examples? The [Recipes](#recipes-the-backup-cookbook) cover Docker, Dokploy, PostgreSQL, MySQL/MariaDB, MongoDB, Redis, SQLite, VirtFusion and whole-server setups.

## Install: back it up, then back it in

Everything happens **on the server**, so it doesn't matter whether your own machine runs Windows, macOS or Linux. SSH in (PowerShell, Windows Terminal, PuTTY, whatever you like), then:

```sh
git clone https://github.com/CodeMeAPixel/NoBackups
cd NoBackups
sudo sh install.sh
```

If the server has Go 1.24+, the installer builds from source. Otherwise it downloads the release binary for the server's CPU and verifies its checksum. Upgrading is `git pull && sudo sh install.sh`.

Want the installer to build from source? Install Go 1.24+ first (as root). Distro packages like `apt install golang` are usually too old, so use the official release:

```sh
GO_VERSION=$(curl -fsSL "https://go.dev/VERSION?m=text" | head -1)
case $(uname -m) in
  x86_64)  GO_ARCH=amd64 ;;
  aarch64) GO_ARCH=arm64 ;;
  armv7l)  GO_ARCH=armv6l ;;
  *)       GO_ARCH=$(uname -m) ;;
esac
curl -fsSLO "https://go.dev/dl/${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
rm -rf /usr/local/go && tar -C /usr/local -xzf "${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
rm "${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
echo 'export PATH=$PATH:/usr/local/go/bin' > /etc/profile.d/go.sh
. /etc/profile.d/go.sh
go version
```

Go is optional; see the [installation docs](docs/installation.mdx) for details.

Other options:

```sh
# no clone: download and verify the latest release
curl -fsSL https://raw.githubusercontent.com/CodeMeAPixel/NoBackups/master/install.sh | sudo sh

# offline: upload a release binary (WinSCP, FileZilla or scp), then on the server
sudo ./nobackups-linux-amd64 install

# from macOS / Linux / WSL with Go and make: build locally and push over ssh
make deploy HOST=root@web01
```

Just want to kick the tyres first? `make demo` (Linux, macOS or WSL) runs a full encrypted backup and restore on local disk. No bucket required.

Every option ends with `nobackups install`, which puts the binary in `/usr/local/bin`. It creates `/etc/nobackups/config.yaml` (from the built-in example) and `/etc/nobackups/nobackups.env` (for secrets), and installs the systemd unit.

```sh
sudo nano /etc/nobackups/config.yaml /etc/nobackups/nobackups.env
sudo nobackups validate            # check the config
sudo nobackups check               # write/list/delete a test object on every destination
sudo nobackups run --all           # first backup
sudo systemctl enable --now nobackups
```

## Configuration: tell it what matters

A minimal config:

```yaml
destinations:
  hetzner:
    type: hetzner
    region: fsn1
    bucket: my-backups
    access_key_id: ${HETZNER_ACCESS_KEY}
    secret_access_key: ${HETZNER_SECRET_KEY}
    prefix: servers/{hostname}

jobs:
  - name: system
    sources: [/etc, /home, /root]
    exclude: ["*.tmp", ".cache", /home/*/Downloads]
    destinations: [hetzner]
    schedule: "15 3 * * *"
    one_file_system: true
    encryption:
      passphrase: ${BACKUP_PASSPHRASE}
    retention:
      keep_last: 7
      keep_days: 30
```

Run `nobackups init -c ./config.yaml` to get the fully commented example, or read [`cmd/nobackups/example.yaml`](cmd/nobackups/example.yaml).

**Secrets.** Any value can use `${VAR}` or `${VAR:-default}`. Variables come from the environment and from `nobackups.env` next to the config file. If a variable is referenced but not defined, config loading fails, so a typo can't silently leave a credential empty.

### Destinations: where your data goes on holiday

| Field | Applies to | Notes |
|---|---|---|
| `type` | all | `local`, `s3`, or a provider shorthand (`alarik`, `rustfs`, `minio`, `garage`, `hetzner`, `aws`, `r2`, `b2`, `digitalocean`, `wasabi`) that fills in that provider's defaults |
| `prefix` | all | Key prefix / subdirectory. `{hostname}` is replaced by the server's hostname, so one bucket can serve a whole fleet. |
| `endpoint` | s3 | Host, optionally with port. Prefix with `http://` for plain HTTP. Derived from `region` for `hetzner`, `aws`, `b2`, `digitalocean` and `wasabi`. |
| `bucket`, `region` | s3 | `region` defaults to `us-east-1`. Most self-hosted stores ignore it. |
| `access_key_id`, `secret_access_key`, `session_token` | s3 | |
| `path_style` | s3 | Defaults to `true` for `alarik`, `rustfs`, `minio` and `garage`; otherwise picked automatically |
| `storage_class` | s3 | e.g. `STANDARD_IA` on AWS |
| `part_size_mb` | s3 | Multipart chunk size, default 64. Max object size is 10,000 × this. |
| `ca_file` / `insecure_skip_verify` | s3 | For private CAs / self-signed certs |
| `path` | local | Absolute directory |

Provider examples:

```yaml
alarik:    { type: alarik, endpoint: s3.example.com, bucket: b, ... }
hetzner:   { type: hetzner, region: nbg1, bucket: b, ... }
rustfs:    { type: rustfs, endpoint: "http://10.0.0.5:9000", bucket: b, ... }
aws:       { type: aws, region: eu-central-1, bucket: b, ... }
r2:        { type: r2, endpoint: <account>.r2.cloudflarestorage.com, bucket: b, ... }
b2:        { type: b2, region: eu-central-003, bucket: b, ... }
other:     { type: s3, endpoint: s3.example.net, region: us-east-1, bucket: b, ... }
```

### Jobs: what to save, and when

| Field | Notes |
|---|---|
| `name` | Letters, digits, `.`, `_`, `-` |
| `sources` | Absolute paths (files or directories) |
| `exclude` | Patterns without `/` match a name anywhere (`*.log`, `node_modules`). Patterns with `/` match full paths and everything under them (`/var/lib/docker`, `/home/*/.cache`). |
| `destinations` | Names from `destinations:` |
| `schedule` | Cron (`m h dom mon dow`), `@daily`, `@hourly`, `@every 6h`. Leave it out for manual-only jobs. |
| `compression` | `zstd` (default), `gzip`, `none`. `compression_level` is optional. |
| `encryption` | One of `passphrase`, `passphrase_file`, or `recipients` (age public keys). With recipients, the server holds no secret. Set `identity_file` (or pass `--identity`) only on the machine you restore from. |
| `retention` | `keep_last` and/or `keep_days`. A snapshot is kept if it meets either rule. The newest snapshot is never deleted. |
| `hooks.before` / `hooks.after` | Shell commands. A failing `before` hook aborts the run. `after` hooks always run and get `$NOBACKUPS_STATUS` (`success`/`failure`). Every hook gets `$NOBACKUPS_JOB` and `$NOBACKUPS_HOST`. |
| `timeout` | e.g. `6h` |
| `one_file_system` | Don't cross into other mounts |
| `notify` | Per-job `discord` / `webhooks` targets, sent in addition to the global ones (see [Notifications](#notifications-no-news-is-suspicious-news)) |

### Notifications: no news is suspicious news

Every job run ends in one of three events: **`success`**, **`warning`** (the backup finished, but some files couldn't be read and were skipped), or **`failure`**. Choose which events each target receives with `on`. Subscribing to `success` also delivers warnings.

**Discord.** Create a webhook (*Server Settings → Integrations → Webhooks → New Webhook*), copy the URL into `nobackups.env` as `DISCORD_WEBHOOK_URL`, then:

```yaml
# for every job
notify:
  discord:
    - url: ${DISCORD_WEBHOOK_URL}
      on: [failure, warning]        # default; add success to hear about every run
      mention: "<@&123456789>"      # optional: ping a role (or "<@USER_ID>", "@here") on failure/warning

jobs:
  - name: postgres
    # ...
    # just for this job, in addition to the global targets above
    notify:
      discord:
        - url: ${DISCORD_DB_TEAM_WEBHOOK}
          on: [failure, success]
```

Each message is a colour-coded embed (green ✅, yellow ⚠️, red ❌). It shows the host, duration, file count, original → stored size, and every destination's result, including how many old snapshots were pruned. On failure it also includes the error message. Mentions only ping on failure and warning, never on success.

**Generic webhooks** receive a JSON POST with `event`, a human-readable `text` (which Slack and Mattermost display directly), and the full result. `on` defaults to `[failure]`.

```yaml
notify:
  webhooks:
    - url: https://hooks.slack.com/services/XXX
      on: [failure]
```

**Test your setup** without waiting for a backup. This sends a sample notification to every target (global plus the job's), ignoring `on` filters:

```sh
nobackups test-notify                      # global targets, simulated failure
nobackups test-notify postgres --event success
```

A notification that can't be delivered is logged, but it never fails the backup.

> **Keep your encryption key somewhere other than the server.** If the server dies and the key was only on it, you can't decrypt the backups. A backup you can't decrypt is just very expensive random noise.

## Usage

```
nobackups [-c config] <command>

  init                      Write an example config file
  validate                  Check the config file
  check                     Test every destination (bucket exists, write/list/delete)
  list                      Show jobs, destinations and next run times
  run [job...] | --all      Run backup jobs now
  daemon                    Run jobs on their schedules (used by systemd)
  snapshots <job>           List stored snapshots
  restore <job> --target D  Restore a snapshot into directory D
  prune [job...]            Apply retention now
  status [--json]           Result of each job's last run
  test-notify [job]         Send a sample notification (--event success|warning|failure)
```

Restore examples:

```sh
nobackups snapshots system
nobackups restore system --target /tmp/restore                          # latest, from first destination
nobackups restore system --target /tmp/restore --snapshot 20261004T031500Z --dest rustfs
nobackups restore system --target /tmp/restore --path etc/nginx         # just one directory
nobackups restore system --target / --path etc/nginx                    # straight back into place
```

Archives keep absolute paths without the leading `/` (like GNU tar). So `/etc/nginx` restored into `/tmp/restore` ends up at `/tmp/restore/etc/nginx`. Permissions and mtimes are restored, and ownership too when running as root.

## Recipes: the backup cookbook

Copy-paste job configs for common workloads, prepped and ready to serve. Each goes under `jobs:` and assumes a destination named `offsite` (rename it to match yours).

### Rule zero: don't copy a moving target

Copying the files of a running database gives you a backup that may not restore. The pattern used throughout these recipes:

1. A `before` hook writes a consistent **dump** (or stops/pauses the service) into a staging directory such as `/var/backups/nobackups/<job>`.
2. NoBackups archives that directory.
3. An `after` hook deletes the dump (or restarts the service). `after` hooks **always run**, even if the backup failed, so services never stay stopped.

Some rules for hooks:

- Each list item runs as its own `sh -c` command, and any failure aborts the job. For a multi-line script (`- |`), **start it with `set -eu`**, otherwise only the last line's exit code counts.
- Dumps are written to local disk first, so the staging directory needs room for one dump. Compression and encryption happen on the way out.
- Create the staging directory with `umask 077` so dumps (which contain all your data) are readable by root only.

---

### Docker: contain your enthusiasm (and your volumes)

**Simplest consistent option:** stop the stack, copy everything, start it again. Downtime lasts as long as the backup takes.

```yaml
- name: docker-app
  sources:
    - /opt/myapp                          # compose.yaml, .env, bind-mounted data
    - /var/lib/docker/volumes/myapp_data  # named volume(s)
  destinations: [offsite]
  schedule: "0 4 * * *"
  hooks:
    before:
      - docker compose -f /opt/myapp/compose.yaml stop
    after:
      - docker compose -f /opt/myapp/compose.yaml start
  retention: { keep_last: 14 }
```

**No downtime:** for containers that only hold plain files (uploads, configs, static sites), back up the volumes live. For containers running databases, dump the database out of the container instead (see below) and exclude its raw data directory.

```yaml
- name: docker-volumes
  sources:
    - /var/lib/docker/volumes
    - /opt                                # wherever your compose projects live
  exclude:
    - /var/lib/docker/volumes/*postgres*  # dumped separately
    - /var/lib/docker/volumes/*mysql*
    - /var/lib/docker/volumes/*mariadb*
    - /var/lib/docker/volumes/backingFsBlockDev
  destinations: [offsite]
  schedule: "30 3 * * *"
  hooks:
    before:
      # Record each container's config so you know how to recreate it
      - |
        set -eu
        umask 077
        mkdir -p /var/backups/nobackups/docker
        docker ps -aq | xargs -r docker inspect > /var/backups/nobackups/docker/containers.json
        docker volume ls --format '{{.Name}}' > /var/backups/nobackups/docker/volumes.txt
  retention: { keep_last: 7, keep_days: 30 }
```

Add `/var/backups/nobackups/docker` to `sources` if you want those inspect files included.

> Don't back up `/var/lib/docker` as a whole. Image layers and container filesystems are huge and can be re-pulled. Back up volumes, bind mounts and compose files. If you build custom images that aren't stored in a registry, save them with `docker save myimage:tag | zstd > /var/backups/nobackups/images/myimage.tar.zst`.

**Restore a volume:**

```sh
docker compose -f /opt/myapp/compose.yaml down
nobackups restore docker-app --target / --path var/lib/docker/volumes/myapp_data
docker compose -f /opt/myapp/compose.yaml up -d
```

---

### Dokploy: deploy with confidence, restore with even more

On a Dokploy server nearly everything lives in Docker, so the whole-server job below (which skips `/var/lib/docker`) backs up **none of your apps** on its own. Add these two jobs.

**The panel:** `/etc/dokploy` plus a consistent dump of Dokploy's own Postgres, done the same way Dokploy's web-server backup does it:

```yaml
- name: dokploy
  sources:
    - /etc/dokploy
    - /var/backups/nobackups/dokploy
  exclude:
    - /etc/dokploy/volume-backups
    - /etc/dokploy/logs
  destinations: [offsite]
  schedule: "0 3 * * *"
  encryption:
    passphrase: ${BACKUP_PASSPHRASE}
  hooks:
    before:
      - |
        set -eu
        umask 077
        mkdir -p /var/backups/nobackups/dokploy
        c=$(docker ps --filter name=dokploy-postgres --filter status=running -q | head -n 1)
        [ -n "$c" ] || { echo "dokploy-postgres is not running"; exit 1; }
        docker exec "$c" pg_dump -Fc -U dokploy -d dokploy > /var/backups/nobackups/dokploy/dokploy.dump
    after:
      - rm -f /var/backups/nobackups/dokploy/dokploy.dump
  retention: { keep_last: 14 }
```

**Your apps:** named Docker volumes, minus the panel database's raw files (the dump above covers it):

```yaml
- name: dokploy-volumes
  sources: [/var/lib/docker/volumes]
  exclude:
    - /var/lib/docker/volumes/dokploy-postgres
    - /var/lib/docker/volumes/backingFsBlockDev
  destinations: [offsite]
  schedule: "30 3 * * *"
  encryption:
    passphrase: ${BACKUP_PASSPHRASE}
  retention: { keep_last: 7, keep_days: 30 }
```

Volumes are copied live. For databases you created *in* Dokploy, turn on Dokploy's own scheduled database backups (they can upload to the same bucket) or dump them with a hook as in the recipes below. Restore steps are in the [Dokploy docs page](docs/recipes/dokploy.mdx).

---

### PostgreSQL: dump it like it's hot

**Installed on the host:** `pg_dumpall` takes a consistent snapshot of every database, plus roles and permissions, without locking writers.

```yaml
- name: postgres
  sources: [/var/backups/nobackups/postgres]
  destinations: [offsite]
  schedule: "0 */6 * * *"
  compression: zstd
  hooks:
    before:
      - |
        set -eu
        umask 077
        mkdir -p /var/backups/nobackups/postgres
        cd /tmp && sudo -u postgres pg_dumpall --clean --if-exists > /var/backups/nobackups/postgres/all.sql
    after:
      - rm -f /var/backups/nobackups/postgres/all.sql
  retention: { keep_last: 28 }
```

For big databases, use one custom-format dump per database instead. These dump in parallel, and `pg_restore` can restore a single table:

```yaml
    before:
      - |
        set -eu
        umask 077
        D=/var/backups/nobackups/postgres
        install -d -m 700 -o postgres "$D"   # pg_dump -Fd writes as the postgres user
        cd /tmp
        sudo -u postgres pg_dumpall --globals-only > "$D/globals.sql"
        for db in $(sudo -u postgres psql -Atc "SELECT datname FROM pg_database WHERE NOT datistemplate"); do
          sudo -u postgres pg_dump -Fd -j 4 -Z 0 -f "$D/$db.dir" "$db"
        done
    after:
      - rm -rf /var/backups/nobackups/postgres/*
```

(`-Z 0` turns off pg_dump's own compression, because NoBackups compresses with zstd anyway.)

**In Docker:**

```yaml
    before:
      - |
        set -eu
        umask 077
        mkdir -p /var/backups/nobackups/postgres
        docker exec my-postgres pg_dumpall -U postgres --clean --if-exists > /var/backups/nobackups/postgres/all.sql
```

**Restore:**

```sh
nobackups restore postgres --target /tmp/r
psql -U postgres -f /tmp/r/var/backups/nobackups/postgres/all.sql
# docker: docker exec -i my-postgres psql -U postgres < /tmp/r/var/backups/nobackups/postgres/all.sql
```

---

### MySQL / MariaDB: single transaction, zero drama

`--single-transaction` gives a consistent dump of InnoDB tables without locking. Keep credentials in `/root/.my.cnf` (`chmod 600`), not on the command line:

```ini
# /root/.my.cnf
[client]
user=backup
password=secret
```

```yaml
- name: mysql
  sources: [/var/backups/nobackups/mysql]
  destinations: [offsite]
  schedule: "0 */6 * * *"
  hooks:
    before:
      - |
        set -eu
        umask 077
        mkdir -p /var/backups/nobackups/mysql
        mysqldump --defaults-extra-file=/root/.my.cnf --all-databases \
          --single-transaction --quick --routines --triggers --events \
          > /var/backups/nobackups/mysql/all.sql
    after:
      - rm -f /var/backups/nobackups/mysql/all.sql
  retention: { keep_last: 28 }
```

On MariaDB 11+ the tool is called `mariadb-dump` (same flags). **In Docker**, the official images keep the root password in the container's environment:

```yaml
    before:
      - |
        set -eu
        umask 077
        mkdir -p /var/backups/nobackups/mysql
        docker exec my-mysql sh -c 'exec mysqldump -uroot -p"$MYSQL_ROOT_PASSWORD" --all-databases --single-transaction --routines --triggers --events' \
          > /var/backups/nobackups/mysql/all.sql
```

(Use `$MARIADB_ROOT_PASSWORD` and `mariadb-dump` for the `mariadb` image.)

**Restore:** `mysql --defaults-extra-file=/root/.my.cnf < /tmp/r/var/backups/nobackups/mysql/all.sql`

---

### MongoDB: no humongous downtime

```yaml
- name: mongodb
  sources: [/var/backups/nobackups/mongodb]
  destinations: [offsite]
  schedule: "0 */6 * * *"
  compression: none          # mongodump --gzip already compresses
  hooks:
    before:
      - |
        set -eu
        umask 077
        mkdir -p /var/backups/nobackups/mongodb
        mongodump --uri="mongodb://backup:${MONGO_BACKUP_PASSWORD}@localhost:27017/?authSource=admin" \
          --archive=/var/backups/nobackups/mongodb/dump.archive.gz --gzip
        # on a replica set, add --oplog for a point-in-time consistent dump
    after:
      - rm -f /var/backups/nobackups/mongodb/dump.archive.gz
```

`${MONGO_BACKUP_PASSWORD}` is filled in from `nobackups.env` when the config loads. In Docker: `docker exec my-mongo mongodump --archive --gzip -u root -p "$PASS" > /var/backups/nobackups/mongodb/dump.archive.gz`.

**Restore:** `mongorestore --archive=dump.archive.gz --gzip --drop`

---

### Redis / Valkey: in-memory, not out of mind

`redis-cli --rdb` asks the server for a fresh snapshot and writes it locally:

```yaml
- name: redis
  sources: [/var/backups/nobackups/redis]
  destinations: [offsite]
  schedule: "0 * * * *"
  hooks:
    before:
      - |
        set -eu
        umask 077
        mkdir -p /var/backups/nobackups/redis
        redis-cli --rdb /var/backups/nobackups/redis/dump.rdb
        # with a password: REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli --rdb ...
        # docker (Redis 7+): docker exec my-redis redis-cli --rdb - > /var/backups/nobackups/redis/dump.rdb
    after:
      - rm -f /var/backups/nobackups/redis/dump.rdb
  retention: { keep_last: 48 }
```

**Restore:** stop Redis, copy `dump.rdb` into its data directory (`/var/lib/redis`), start Redis.

---

### SQLite: small database, big regrets if you lose it

This also covers apps built on it: Vaultwarden, Gitea, Uptime Kuma, Home Assistant…

Copying a live SQLite file can catch it mid-write. `sqlite3 .backup` makes a safe copy without stopping the app:

```yaml
- name: vaultwarden
  sources:
    - /opt/vaultwarden/data
    - /var/backups/nobackups/vaultwarden   # the safe copy made by the hook
  exclude: ["db.sqlite3*"]                 # live DB + WAL files from the data dir
  destinations: [offsite]
  schedule: "0 */4 * * *"
  hooks:
    before:
      - |
        set -eu
        umask 077
        mkdir -p /var/backups/nobackups/vaultwarden
        sqlite3 /opt/vaultwarden/data/db.sqlite3 ".backup '/var/backups/nobackups/vaultwarden/vaultwarden.sqlite3'"
```

---

### VirtFusion: keep your panel from becoming a panic

> The paths below are VirtFusion's defaults. Check them on your install before relying on them.

**Control server.** This is the panel itself: its database plus the application directory with its config/`.env`. Losing it means losing every server, user and package definition, so back it up often and send it offsite:

```yaml
- name: virtfusion-control
  sources:
    - /opt/virtfusion                       # app, config, .env
    - /var/backups/nobackups/virtfusion
  exclude: ["*.log"]
  destinations: [offsite]
  schedule: "0 */6 * * *"
  encryption:
    passphrase: ${BACKUP_PASSPHRASE}        # the .env holds secrets: always encrypt
  hooks:
    before:
      - |
        set -eu
        umask 077
        mkdir -p /var/backups/nobackups/virtfusion
        # DB name/credentials are in the app's .env (look for DB_DATABASE / DB_USERNAME)
        mysqldump --defaults-extra-file=/root/.my.cnf --single-transaction --routines --triggers \
          virtfusion > /var/backups/nobackups/virtfusion/virtfusion.sql
    after:
      - rm -f /var/backups/nobackups/virtfusion/virtfusion.sql
  retention: { keep_last: 28, keep_days: 30 }
```

**Hypervisors.** Back up the node's own configuration. That's enough to rebuild the node and reconnect it to the panel:

```yaml
- name: virtfusion-hypervisor-config
  sources: [/etc, /root, /opt/virtfusion]
  exclude: [/etc/shadow-, /etc/gshadow-]
  destinations: [offsite]
  schedule: "0 2 * * *"
  one_file_system: true
  encryption:
    passphrase: ${BACKUP_PASSPHRASE}
  retention: { keep_last: 14 }
```

**VM disks.** Don't point NoBackups at the live disk images of running VMs (e.g. under `/home/vf-data`). A disk being written to while it's copied can produce an image that won't boot. Instead, use VirtFusion's built-in backup plans, which snapshot VMs properly. If those plans write to a local or NFS directory, add that directory to a NoBackups job to get the backups offsite:

```yaml
- name: virtfusion-vm-backups-offsite
  sources: [/home/vf-data/backups]          # wherever your VirtFusion backup plan writes
  destinations: [offsite]
  schedule: "0 6 * * *"                     # after VirtFusion's own backup window
  compression: none                          # disk images are usually already compressed
  retention: { keep_last: 7 }
```

---

### Whole server: the "just take everything" button

A "disaster recovery" job for a server without special workloads. `one_file_system` keeps it out of `/proc`, `/sys`, `/dev`, tmpfs and network mounts:

```yaml
- name: full
  sources: [/]
  one_file_system: true
  exclude:
    - /tmp
    - /var/tmp
    - /var/cache
    - /var/lib/docker                 # back up volumes in a dedicated job instead
    - /var/lib/containerd
    - /var/lib/mysql                  # databases: use dumps (see above)
    - /var/lib/postgresql
    - /var/backups/nobackups          # staging area of other jobs
    - /swapfile
    - /home/*/.cache
    - "*.log"
  destinations: [offsite]
  schedule: "0 1 * * 0"               # weekly
  encryption:
    passphrase: ${BACKUP_PASSPHRASE}
  retention: { keep_last: 4 }
```

On Docker hosts (Dokploy, Coolify, Portainer…) this job skips all of your apps' data. Pair it with the Docker or Dokploy recipe above.

If `/home` or `/var` are separate filesystems, list them as extra sources, because `one_file_system` won't cross into them from `/`.

---

### Web servers & mail: you've got (backed-up) mail

```yaml
- name: web
  sources: [/etc/nginx, /etc/letsencrypt, /var/www]
  exclude: [node_modules, .git, "/var/www/*/cache"]
  destinations: [offsite]
  schedule: "0 3 * * *"
  retention: { keep_last: 7, keep_days: 30 }

- name: mail
  sources: [/var/vmail, /etc/postfix, /etc/dovecot]   # Maildir is safe to copy live
  destinations: [offsite]
  schedule: "0 */12 * * *"
```

---

### Getting notified: silence isn't golden

Add Discord (see [Notifications](#notifications-no-news-is-suspicious-news)) so failed backups don't go unnoticed. A dead man's switch such as healthchecks.io also tells you when backups *stop running entirely*, which no failure message can:

```yaml
notify:
  discord:
    - url: ${DISCORD_WEBHOOK_URL}           # failures and warnings
  webhooks:
    - url: https://hc-ping.com/your-uuid    # alerts you if no success ping arrives on time
      on: [success]
```

## Storage layout & manual recovery: no lock-in, no lock-out

```
<bucket>/<prefix>/<job>/<job>-20261004T031500Z.tar.zst.age
```

Each snapshot is a self-contained standard file, so you can recover without NoBackups:

```sh
age -d backup.tar.zst.age | zstd -d | tar -x -C /tmp/restore
```

## Operations: day two (and day two thousand)

- **Logs**: `journalctl -u nobackups -f`
- **Reload config** without restarting: `systemctl reload nobackups` (sends SIGHUP; an invalid config is rejected and the old one stays active)
- **Monitoring**: Discord/webhook notifications, `nobackups status --json`, or the per-job files in `/var/lib/nobackups/status/`
- A scheduled run that fires while the previous run of that job is still going is skipped and logged.

## Development: backing up our claims

Everything goes through `make`. Run `make` on its own to see the full menu:

| Target | What it does |
|---|---|
| `make build` | Build `./nobackups` for this machine |
| `make dist` | Static binaries for linux amd64, arm64 and armv7 in `dist/` (`PLATFORMS="linux/amd64"` to narrow it) |
| `make checksums` | `dist` + `dist/SHA256SUMS` |
| `make run ARGS="validate"` | Build and run against `CONFIG` (default `./config.yaml`) |
| `make demo` | Local-disk backup → restore round trip, no S3 needed |
| `make test` | All tests with the race detector, including end-to-end tests against an in-process S3 server (we test our restores, you should too) |
| `make test-short` | Fast tests only |
| `make cover` | Coverage report in `coverage.html` |
| `make fmt` / `make lint` | Format, or check formatting + `go vet` (+ `staticcheck` if installed) |
| `make check` | Lint + tests, the same as CI. Run it before you push |
| `make install` / `make uninstall` | Build, then run `nobackups install` / `uninstall` on this machine. Supports `PREFIX=/usr` and `DESTDIR=` for packaging. Uninstall keeps config, state and backups. |
| `make deploy HOST=user@server` | Cross-compile for `ARCH`, copy the binary over ssh and run `nobackups install` there. Use `SUDO=` when logging in as root on a host without sudo. |
| `make clean` | Remove build output |

To release, open **Actions → CI → Run workflow** on GitHub and enter a version like `v0.1.0`, or push a `v*` tag. The workflow runs the checks, then publishes the binaries, `SHA256SUMS` and `install.sh` as a GitHub release with generated notes. The installer's no-Go path downloads from the latest release, so publish one before sharing the one-liner.

---

<sub>NoBackups: because "we'll set up backups next sprint" is how every outage post-mortem starts.</sub>
