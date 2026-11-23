# mongodb-backup-s3

A Go tool for backing up MongoDB data, authentication information, and configuration to S3 or S3-compatible storage.
It inherits S3 transfers, optional GPG encryption, scheduling, retention, fixed filenames, and restoration by S3 object version
from `mariadb-backup-s3-main`. The source directory is left unchanged.
Original licenses and attribution are preserved in `LICENSE` and `LICENSE.txt`.

| `BACKUP_MODE` | Backup unit | Writes during backup |
| --- | --- | --- |
| `full` (default) | One object for the entire replica set | Uses `--oplog` and applies captured changes with `--oplogReplay` during restore |
| `per-database` | One object per selected database | No oplog; point-in-time consistency is not guaranteed, even within a database or collection |

Designed for self-managed replica sets. Sharded clusters, standalone servers, and continuous point-in-time recovery are not supported.
`full` does not accept database or collection filters or exclusions. MongoDB's `--oplog` requires a full dump.
[MongoDB backup documentation](https://www.mongodb.com/docs/database-tools/mongodump/)

## Usage

Supports Linux amd64 / arm64 and includes `mongodump` and `mongorestore` from MongoDB Database Tools **100.19.1**.
Integration tests target MongoDB 8.0. Verify restores with the same Server / FCV / Tools combination used in production.

The image uses a custom distroless runtime built from `scratch`, with MongoDB Tools, GPG,
their matching Ubuntu libraries, CA certificates, and timezone data. It contains no shell or package manager.
**Breaking change:** `/run.sh`, `/backup.sh`, and `/restore.sh` have been removed.
Replace them with `mongodb-backup-s3 run`, `mongodb-backup-s3 backup`, and `mongodb-backup-s3 restore` respectively.
The default command remains `mongodb-backup-s3 run`.

After the first image is published to GHCR, run it with the following commands.
`latest` tracks builds from `main`. Use a published `v*` tag to pin a version.
For private packages, run `docker login ghcr.io` first.

```sh
docker pull ghcr.io/ajctrl/mongodb-backup-s3:latest
cp template.env .env
# Set the MongoDB connection details and an existing S3 bucket in .env
docker run --rm --env-file .env ghcr.io/ajctrl/mongodb-backup-s3:latest
```

The default `run` command runs once and exits when `SCHEDULE` is empty.
With `SCHEDULE=CRON_TZ=Asia/Tokyo 0 3 * * *`, it waits until the next scheduled time and runs daily at 03:00.
Use `backup` to run immediately regardless of the schedule.

```sh
docker run --rm --env-file .env ghcr.io/ajctrl/mongodb-backup-s3:latest mongodb-backup-s3 backup
```

Scheduled runs do not overlap within the same process. Do not run multiple containers writing to the same destination.
Failures are logged, and one-off runs exit with a nonzero status on failure.

### Docker Compose

Add the following `backup` service to your `compose.yml` for an existing MongoDB replica set.
Replace the example hostnames, replica set name, bucket, region, and prefix with your own values.
Ensure the container can reach every replica set member advertised by MongoDB; attach it to your MongoDB network if needed.

```yaml
services:
  backup:
    image: ghcr.io/ajctrl/mongodb-backup-s3:latest
    restart: unless-stopped
    environment:
      MONGODB_URI: "mongodb://backup:${MONGODB_PASSWORD:?Set MONGODB_PASSWORD}@mongo1.example.com:27017,mongo2.example.com:27017,mongo3.example.com:27017/?replicaSet=rs0&authSource=admin"
      MONGODB_READ_PREFERENCE: primary
      MONGODB_PARALLEL_COLLECTIONS: "4"
      BACKUP_MODE: full
      S3_BUCKET: my-backup-bucket
      S3_REGION: us-west-1
      S3_PREFIX: backup
      S3_ACCESS_KEY_ID: "${S3_ACCESS_KEY_ID:?Set S3_ACCESS_KEY_ID}"
      S3_SECRET_ACCESS_KEY: "${S3_SECRET_ACCESS_KEY:?Set S3_SECRET_ACCESS_KEY}"
      S3_SESSION_TOKEN: "${S3_SESSION_TOKEN:-}"
      S3_ENDPOINT: "${S3_ENDPOINT:-}"
      SCHEDULE: "CRON_TZ=Asia/Tokyo 0 2 * * *"
      BACKUP_KEEP_DAYS: "7"
      BACKUP_FILENAME_MODE: timestamp
      PASSPHRASE: "${PASSPHRASE:-}"
```

Create `.env` next to `compose.yml` with the credentials below. Compose substitutes these values into `environment`.
`MONGODB_PASSWORD` is used only for Compose substitution; the tool receives the resulting `MONGODB_URI`.
URL-encode reserved characters in the MongoDB password before setting it here.
The `backup` user must already have the `backup` and `clusterMonitor` roles on `admin` (see below).

```dotenv
MONGODB_PASSWORD=CHANGE_ME
S3_ACCESS_KEY_ID=CHANGE_ME
S3_SECRET_ACCESS_KEY=CHANGE_ME
# Optional: leave empty to disable GPG encryption
PASSPHRASE=
```

```sh
docker compose -f compose.yml up -d backup
# Run a backup immediately
docker compose -f compose.yml run --rm backup mongodb-backup-s3 backup
```

This example backs up the entire replica set, including users, roles, and oplog, daily at 02:00 Asia/Tokyo.
After a successful backup, timestamped backups older than seven days are deleted from the destination.
Use a separate `S3_PREFIX` for each replica set. For S3-compatible storage, set `S3_ENDPOINT` in `.env`;
for temporary AWS credentials, also set `S3_SESSION_TOKEN`.
To back up only selected databases, set `BACKUP_MODE: per-database` and add `MONGODB_DATABASES: '["app","analytics"]'`
to `environment`; this mode does not include oplog (see [Per-database backups](#per-database-backups)).
Run an immediate backup only when no scheduled backup is running.
To include local configuration files, add `MONGODB_CONFIG_DIR` to the existing `environment` mapping
and add the read-only mount to the `backup` service. Populate `./backup-config` with the files to include first.

```yaml
    environment:
      # Keep the other environment settings above.
      MONGODB_CONFIG_DIR: /backup-config
    volumes:
      - ./backup-config:/backup-config:ro
```

## MongoDB connection and permissions

Leave the database path empty in the URI and set the authentication database with `authSource`.
URL-encode reserved characters in usernames and passwords.

```dotenv
MONGODB_URI=mongodb://backup:CHANGE_ME@mongo1:27017,mongo2:27017,mongo3:27017/?replicaSet=rs0&authSource=admin
MONGODB_READ_PREFERENCE=primary
```

To supply the URI through a secret file, leave `MONGODB_URI` empty, set `MONGODB_URI_FILE=/run/secrets/mongodb_uri`,
and mount the file read-only. Setting both variables is an error.
The URI is passed to child processes through a temporary configuration file with `0600` permissions, rather than command-line arguments.

MongoDB Tools 100.19.1 fixes an earlier bug that interpreted URI timeout milliseconds as seconds.
**Breaking change:** when specified, `serverSelectionTimeoutMS` and `connectTimeoutMS` must be positive
multiples of 1,000 (for example, `2000` for two seconds). Zero, fractional seconds, repeated values, and
out-of-range values are rejected because the tools only preserve whole seconds.
These options control MongoDB Tools; metadata operations retain their separate 30-second limit.
Custom images must use Tools 100.19.1 or later and pass the timeout integration tests.

**Breaking change:** `socketTimeoutMS` and `timeoutMS` are rejected, including zero or empty values.
MongoDB Tools 100.19.1 accepts these options but does not enforce them; remove them from the URI.
This applies to both `MONGODB_URI` and `MONGODB_URI_FILE`, regardless of option-name capitalization.
Connection and server-selection timeouts do not bound operations after connection, and this application
does not provide a MongoDB operation timeout. The metadata client's 30-second limit does not apply to
`mongodump` or `mongorestore`. An operation that stops responding may require cancellation with SIGTERM/SIGINT.

Enable TLS with `tls=true` in the URI. For a custom CA, mount the CA file and set `MONGODB_TLS_CA_FILE` to its container path.
Certificates collected as server configuration are not automatically applied to connection settings.

**Breaking change:** `restore` and `restore-auth` reject `w=0` and repeated `w` URI options,
including differently capitalized or percent-encoded forms. Unacknowledged writes can cause
`mongorestore` to report success even when the server rejects documents. Omit `w` to use the tool's
default, or select an acknowledged write concern such as `w=1` or `w=majority`.
This validation also applies to `MONGODB_URI_FILE`; backup and configuration extraction are unaffected.

Grant the backup account the `backup` and `clusterMonitor` roles on `admin`.
`clusterMonitor` is used to retrieve the active replica set configuration and status.
An administrator can create the account in `mongosh`:

```javascript
db.getSiblingDB('admin').createUser({
  user: 'backup',
  pwd: 'CHANGE_ME',
  roles: [
    { role: 'backup', db: 'admin' },
    { role: 'clusterMonitor', db: 'admin' }
  ]
});
```

`MONGODB_READ_PREFERENCE=secondary` directs backup data reads and replica metadata commands to secondaries.
MongoDB Tools still use the primary for database/collection enumeration, so this does not guarantee a backup with the primary unavailable.
`secondaryPreferred` falls back to the primary when no secondary is available.
Check I/O load, replication lag, and oplog retention on the members serving reads.
Restores connect to the writable target; this backup setting does not direct restores to a secondary.

## Per-database backups

```dotenv
BACKUP_MODE=per-database
MONGODB_DATABASE=
MONGODB_DATABASES=["app","analytics"]
MONGODB_BACKUP_ALL=false
MONGODB_DATABASES_EXCLUDE=
```

Use `MONGODB_DATABASE=app` for a single database or `MONGODB_DATABASES=["app","analytics"]` in a Docker env file for multiple databases.
A nonempty `MONGODB_DATABASES` array takes precedence when both are set.
An unset variable, an empty value, or `[]` means no entries.

**Breaking change:** `MONGODB_DATABASES` and `MONGODB_DATABASES_EXCLUDE` now require JSON arrays of strings.
Old comma-separated values are rejected; replace `app,analytics` with `["app","analytics"]`.
Names are preserved exactly: `["private,archive"]` selects or excludes the single database named `private,archive`.
Docker `--env-file` preserves outer quotes literally: use the unquoted JSON arrays shown in these dotenv blocks.
In a shell assignment, use `MONGODB_DATABASES='["app","analytics"]'`; in a Compose YAML mapping,
use `MONGODB_DATABASES: '["app","analytics"]'`. Those outer single quotes are shell/YAML syntax only.
For automatic discovery, clear the explicit selections and use:

```dotenv
BACKUP_MODE=per-database
MONGODB_DATABASE=
MONGODB_DATABASES=
MONGODB_BACKUP_ALL=true
MONGODB_DATABASES_EXCLUDE=["scratch","temp"]
```

`MONGODB_BACKUP_ALL=true` cannot be combined with explicit selections. Exclusions apply only to automatic discovery.
Discovery excludes `admin`, `config`, and `local`; explicitly selecting these internal databases is also rejected.
Names follow MongoDB's Linux restrictions, so names such as `legacy:db` and `star*db` are supported.
Exclusions are applied before validating the selected databases. Literal stars in names are escaped in
`mongorestore` namespace filters so they do not select other databases.
Each database bundle includes authentication information in a separate `auth.archive.gz`.
This is **a dump of the admin database containing users and roles for all databases**, not just the selected database.
Data and authentication information are captured sequentially for each database, so they are not a single point-in-time snapshot.

Writes during a `per-database` backup may produce a mixture of data from before and after updates.
If consistency is required, stop writes and schema changes to the target databases or use `full`.

## Backing up configuration, keys, and certificates

The active replica set configuration, equivalent to `rs.conf()`, is retrieved from the connected server and saved as `replica-set.json` in every backup.
Local files such as `mongod.conf`, an initialization `rs.conf`, keyFiles, TLS certificates and private keys, and Compose definitions
cannot be retrieved through the MongoDB connection. Collect them in a directory and mount it read-only.

```text
backup-config/
├── compose.yaml
├── rs.conf
├── node1/
│   ├── mongod.conf
│   └── keyFile
├── node2/
│   └── mongod.conf
└── tls/
    ├── ca.pem
    └── server.pem
```

```sh
docker run --rm --env-file .env \
  --mount type=bind,src="$(pwd)/backup-config",dst=/backup-config,readonly \
  -e MONGODB_CONFIG_DIR=/backup-config \
  ghcr.io/ajctrl/mongodb-backup-s3:latest mongodb-backup-s3 backup
```

For members on multiple hosts, collect their configuration files from each host beforehand.
The configuration directory is collected recursively; symbolic links and special files are rejected.
Relative configuration paths must be valid UTF-8. A filename containing invalid byte sequences causes the backup to fail.
Avoid changing configuration during collection. Leaving `MONGODB_CONFIG_DIR` empty skips local file collection.
After collection, the tool rechecks the file list, content hashes, and original directory identity, failing the backup if changes are detected.
This check reads the configuration files again; it is not an atomic filesystem snapshot.
If a configured directory contains unreadable files or is empty, the backup fails.

Dumps include users, roles, and SCRAM authentication information. They do not retrieve plaintext passwords.
Credentials from external authentication systems and files that have not been mounted are not included.
Because backups can contain secrets, set `PASSPHRASE` when needed and store the decryption value separately from S3.

## S3 layout

Data and configuration are **combined into the same ZIP64 bundle and published as one object**.
If dumping, configuration collection, or encryption fails, the incomplete backup is not published as a completed object.
This keeps data and configuration from different backup generations from being mixed.

```text
backup/full/2026-09-13T03:00:00-0123456789abcdef0123456789abcdef.zip.gpg
backup/per-database/app/2026-09-13T03:00:00-0123456789abcdef0123456789abcdef.zip.gpg
backup/per-database/analytics/2026-09-13T03:00:01-fedcba9876543210fedcba9876543210.zip.gpg
```

After decryption, the bundle has the following structure. `data.archive.gz` uses MongoDB's own archive format.

```text
data.archive.gz       # full: all data + oplog; per-database: the selected database
auth.archive.gz       # per-database only: admin database with all users and roles
replica-set.json      # Active replica set configuration
config/...            # Optional mounted configuration files
manifest.json         # Format, version, capture time, file SHA-256 hashes, etc.
```

Unencrypted bundles use `.zip`; encrypted bundles use `.zip.gpg`. Database names in paths are URL-encoded.
Separate directories for each mode keep latest-backup lookup and deletion scoped to that mode.
Use a separate `S3_PREFIX` for each source replica set.
Generation names contain a UTC timestamp and a random 128-bit ID, independent of the scheduling timezone.
The full S3 key is logged when an upload starts. Timestamp-mode uploads use `If-None-Match: *`
on both single-part writes and multipart completion to refuse replacement of an existing key.
The S3 service must support these conditional writes; errors do not fall back to overwriting.
This adds a compatibility requirement for S3-compatible services that did not implement conditional multipart completion.
If the completion time precedes the start time (a wall-clock rollback), the backup fails before publication and retention is skipped.

**Breaking change:** timestamp-only filenames from earlier versions are no longer selected by latest-backup lookup,
accepted as restore arguments, or managed by retention. Restore arguments now use the entire generation name
without `.zip` or `.zip.gpg`. Latest-backup lookup uses S3 `LastModified`. If multiple generations share
the newest S3 timestamp, each tied bundle is downloaded and validated, and the manifest's `completed_at`
selects the latest one. If that completion time is also tied, automatic restore stops and requires an
explicit generation; random IDs no longer break ties. An unreadable or invalid candidate also stops selection.
Fixed-name objects and their version-ID selection retain the same naming format.

`BACKUP_FILENAME_MODE=fixed` overwrites `full/latest.zip[.gpg]` or `per-database/<DB>/latest.zip[.gpg]`.
Before uploading with a fixed filename, the tool checks bucket versioning and stops if it is disabled.
If the check itself fails, for example because of insufficient permissions, the backup stops without uploading.
This changes the previous warning-and-continue behavior; the backup account needs `s3:GetBucketVersioning`.
Enable versioning beforehand.

After a successful backup, `BACKUP_KEEP_DAYS` deletes expired timestamped backups in the target directory.
Expiration compares S3 last-modified times against the newly uploaded object's S3 timestamp, rather than the host clock.
The newly uploaded generation and the newest timestamp generation (including ties) are always retained,
even if their retention period has elapsed. This also preserves the newest timestamp generation when switching to fixed filenames.
Before any deletion, the uploaded object must be present in the listing with a valid timestamp, and all managed
objects must have timestamps. The uploaded object's S3 timestamp must fall within the local backup start/end interval,
with a five-minute tolerance on either side; this accommodates multipart uploads timestamped at initiation.
If these checks fail or the local clock moves backwards, the saved backup remains available, retention performs no deletions,
and the command reports failure (scheduled runs log the failure).
Fixed filenames and previous object versions are not deleted.
Use S3 Lifecycle rules to manage their retention and clean up incomplete multipart uploads.

Backups stream through archive → ZIP → optional GPG encryption → S3.
ZIP64 supports entries larger than 4 GiB without storing the entire dump in memory or on temporary disk.
Even for a 50 GB database, each run is a full logical backup that reads all data.
Duration depends on compression CPU usage, network speed, and database read load.
`full` requires the oplog to cover the entire dump, so allow enough retention for increases in write volume.
Avoid operations that `mongodump --oplog` does not support during a dump, such as renaming collections.

Streaming uploads have an unknown final size, so the SDK cannot automatically increase the part size.
S3 permits at most 10,000 parts: the maximum **compressed/encrypted bundle size** is
`S3_UPLOAD_PART_SIZE_MB × 10,000 MiB`, including configuration and archive overhead.

| `S3_UPLOAD_PART_SIZE_MB` | Maximum output size |
| --- | --- |
| `8` (default) | 78.125 GiB |
| `64` | 625 GiB |
| `128` | 1,250 GiB |

For larger backups, increase the existing `S3_UPLOAD_PART_SIZE_MB` setting before running the backup,
allowing headroom for growth. For example, use `S3_UPLOAD_PART_SIZE_MB=64` for an expected 100 GiB output.
Exceeding the limit fails the upload after reading and transferring data; it does not publish the incomplete
generation or run retention. Increasing the part size also increases RAM use: two parts can upload concurrently,
with additional read buffers and SDK overhead. The setting is not a total memory limit; leave memory for
MongoDB Tools and GPG too. No additional backup-size setting is required.

Database names and S3 keys are quoted and escaped in application logs so control characters cannot create
extra log records. The actual database names and S3 keys are unchanged; log parsers must account for the quotes.

## Restore

Create `restore.env` with the same `S3_PREFIX`, `BACKUP_MODE`, `BACKUP_FILENAME_MODE`, and `PASSPHRASE` used for the backup,
and change the MongoDB URI to the **restore target**.
The target should normally be an already provisioned, empty environment.
A restore is not a transaction that rolls back on failure; partially written data remains if it fails.

The entire S3 object is downloaded, decrypted, and checked for ZIP, manifest, and hash validity before import starts.
Archives are read from the validated ZIP and streamed to `mongorestore` through standard input.
Temporary disk space must accommodate one bundle without encryption, or both the encrypted and decrypted bundles with encryption.
When the latest S3 timestamp is tied, selection can temporarily retain two such bundles while comparing candidates.
No additional space is needed to extract the import archive as a separate file.
Provide enough space through `TMPDIR` or a disk mounted at `/tmp`.
Restore time also includes index rebuilding and oplog replay after import.

```sh
# Latest backup
docker run --rm --env-file restore.env ghcr.io/ajctrl/mongodb-backup-s3:latest mongodb-backup-s3 restore

# Specific generation (filename without the extension)
docker run --rm --env-file restore.env ghcr.io/ajctrl/mongodb-backup-s3:latest \
  mongodb-backup-s3 restore 2026-09-13T03:00:00-0123456789abcdef0123456789abcdef

# Previous version of a fixed-name object
docker run --rm --env-file restore.env -e BACKUP_FILENAME_MODE=fixed \
  ghcr.io/ajctrl/mongodb-backup-s3:latest mongodb-backup-s3 restore --version-id VERSION_ID
```

For `full` restores, clear all database selection variables or set them to false as appropriate.
All data, users, and roles are restored, and the oplog is always replayed. Partial restores and database renaming are not supported.
For `per-database` restores, select one database, such as `MONGODB_DATABASE=app`, and clear the other selection variables.
Only data is restored to the original database name; authentication information is not changed automatically.

Normal restores use `--stopOnError`. `MONGODB_RESTORE_DROP=true` drops existing collections included in the dump before restoring them.
Collections absent from the dump are not dropped.
Restoring the admin database with `--drop` also replaces user information, so credentials from the backup are required afterward.
When `MONGODB_RESTORE_DROP=true` is used with a `full` restore or `restore-auth`, users and roles are restored first,
then the remaining restore uses a new connection. This prevents invalidated authenticated connections from interrupting oplog replay
or admin database index creation when internal user IDs are replaced.
If authentication is enabled, the restore account must exist in both the target and the backup with the same username and password.
Its roles in the backup must also grant the permissions required for the restore, including oplog replay for `full`.
The validated archive is read twice, once for the initial authentication restore and once for the remaining restore, without creating additional extracted files.
[MongoDB restore documentation](https://www.mongodb.com/docs/database-tools/mongorestore/)

### Explicitly restoring authentication information

Use a separate command to restore `auth.archive.gz` from a per-database bundle.
This restores **users and roles for all databases, plus other admin data** contained in the admin database.
It is not limited to users of the selected database.

```sh
docker run --rm --env-file restore.env \
  -e BACKUP_MODE=per-database -e MONGODB_DATABASE=app \
  ghcr.io/ajctrl/mongodb-backup-s3:latest mongodb-backup-s3 restore-auth
```

`restore-auth` also accepts a generation name or `--version-id`. It is not used with `full`, which includes authentication information in the main archive.
Use a restore account separate from the backup account.
The `restore` role is the baseline for ordinary data restores, but oplog replay requires additional permissions.
MongoDB documents a dedicated role with `anyAction` on `anyResource` for oplog replay.
Restoring authentication information, `system.profile`, or other special content also requires permissions appropriate to that content.
[MongoDB restore permissions](https://www.mongodb.com/docs/database-tools/mongorestore/mongorestore-behavior-access-usage/)

### Extracting configuration only

```sh
mkdir -p recovered
docker run --rm --env-file restore.env -e MONGODB_URI= -e MONGODB_URI_FILE= \
  --mount type=bind,src="$(pwd)/recovered",dst=/recovered \
  ghcr.io/ajctrl/mongodb-backup-s3:latest mongodb-backup-s3 extract-config /recovered/generation
```

`extract-config OUTPUT_DIR [GENERATION | --version-id VERSION_ID]` extracts `replica-set.json`, `manifest.json`, and `config/`
without connecting to MongoDB. It rejects an output directory that already exists.
Even when extracting only configuration, it downloads and decrypts the entire bundle and validates every entry before extraction.
Hashes detect changes or corruption. Hashes in an unencrypted ZIP are not signatures proving the source's authenticity.

During recovery, extract the files, adjust settings for the target hostnames, paths, and certificates,
provision the replica set, and then restore the data. The tool does not apply configuration automatically or run `rs.initiate()` or `rs.reconfig()`.
Extracted files have `0600` permissions and directories have `0700` permissions.
Original ownership and permissions are not restored automatically; adjust them when placing the files so the MongoDB process can read them.

## Environment variables

| Variable | Default | Description |
| --- | --- | --- |
| `MONGODB_URI` / `MONGODB_URI_FILE` | Required | Set one; not required for `extract-config` |
| `MONGODB_READ_PREFERENCE` | `primary` | `primary` / `primaryPreferred` / `secondary` / `secondaryPreferred` / `nearest` |
| `MONGODB_TLS_CA_FILE` | Empty | CA file for TLS verification |
| `MONGODB_PARALLEL_COLLECTIONS` | `4` | Dump concurrency and requested restore concurrency; 1–128. During restore, MongoDB Tools may increase this to consume the archive's multiplexed streams; this is not a restore concurrency cap. |
| `MONGODB_CONFIG_DIR` | Empty | Directory containing configuration files to back up |
| `MONGODB_RESTORE_DROP` | `false` | Drop target collections before restoring |
| `BACKUP_MODE` | `full` | `full` / `per-database` |
| `MONGODB_DATABASE` | Empty | Single database selection or per-database restore target |
| `MONGODB_DATABASES` | Empty | JSON array of exact database names to back up; `[]` means no entries |
| `MONGODB_BACKUP_ALL` | `false` | Automatically discover application databases |
| `MONGODB_DATABASES_EXCLUDE` | Empty | JSON array of exact names to exclude from automatic discovery |
| `S3_BUCKET` | Required | Existing destination bucket |
| `S3_REGION` | `us-west-1` | AWS region |
| `S3_PREFIX` | `backup` | Object key prefix; use a separate prefix for each replica set |
| `S3_ENDPOINT` | Empty | HTTP(S) endpoint for S3-compatible storage; uses path-style access |
| `S3_ACCESS_KEY_ID` / `S3_SECRET_ACCESS_KEY` | Empty | Optional credentials; AWS SDK credential providers are also supported |
| `S3_SESSION_TOKEN` | Empty | Token for temporary credentials |
| `S3_UPLOAD_PART_SIZE_MB` | `8` | Multipart part size in MiB; 5–5120. Streaming output limit is 10,000 parts; see the size table above. |
| `SCHEDULE` | Empty | Cron expression, cron with seconds, `@daily`, etc. |
| `BACKUP_KEEP_DAYS` | Empty | Delete expired backups after success; 1–36500 days |
| `BACKUP_FILENAME_MODE` | `timestamp` | `timestamp` / `fixed` |
| `PASSPHRASE` | Empty | Optional GPG symmetric encryption passphrase; newlines are not allowed |

S3 permissions must allow uploading, listing, and downloading objects.
Retention also requires deletion permissions, fixed filenames require checking bucket versioning,
and restores by version ID require access to previous object versions.

## Development

```sh
go test -count=1 -race ./...
go vet ./...
docker build -t mongodb-backup-s3:integration .
go test -count=1 -tags=integration -v -timeout=20m ./tests/integration
```

Integration tests start disposable MongoDB replica set and Silo containers.
CI runs the final image's runtime checks and backup/restore integration suite on native amd64 and arm64
runners. Both architectures must pass before the publishing job runs.
Backup commands use a dedicated account with only `backup` and `clusterMonitor` roles;
fixture setup and restores use a separate administrator. Tests also verify that insufficient privileges
prevent publication and that conditional writes preserve existing generations.
Tests cover literal commas in JSON database selections/exclusions, scheduled recurrence after failures,
overlap prevention, and SIGTERM/SIGINT during schedule waits and active multipart uploads.
Control-character database names are backed up and restored without changing the names, and application
log tests verify escaping on success, failure, retention, and download paths. Streaming upload tests reduce
the SDK's part-count limit to exercise the exact size boundary and one byte over it with multiple part sizes,
including aborting oversized uploads without publication.
Connection tests add a non-voting secondary and reject data and replica metadata reads on the primary while backing up,
so both the Go metadata commands and MongoDB Tools' data reads must use the secondary. The tests repeat with TLS and a freshly
generated private CA, restore to a primary, and reject an unrelated CA and a mismatched hostname in both
backup and restore. Certificates are generated by the tests; no production certificates are used.
Timeout tests run both installed tools against an unresponsive TCP endpoint and measure one- and two-second
server-selection and connection timeouts. In Docker mode these tests use the host network on Linux.
Shutdown checks require the expected exit code, no completed partial backup, aborted uploaded parts,
and released locks. Only disposable test MongoDB instances enable failpoints to hold an active dump.
The default S3 server is the digest-pinned
[`pgsty/silo:RELEASE.2026-10-18T00-00-00Z-distroless`](https://github.com/pgsty/silo/releases/tag/RELEASE.2026-10-18T00-00-00Z)
image. It starts the server directly and does not require a shell or client tools inside the container.
Silo uses `MINIO_ROOT_USER` and `MINIO_ROOT_PASSWORD` for its root credentials.
Override `MONGODB_TEST_IMAGE`, `BACKUP_TEST_IMAGE`, or `S3_TEST_IMAGE` to test another image;
an S3 image override must accept the same server arguments and credential variables.
For local binaries, set `MONGODB_NATIVE_INTEGRATION=1` and optionally
`MONGOD_TEST_BINARY`, `SILO_TEST_BINARY`, and `BACKUP_TEST_BINARY`.
`SILO_TEST_BINARY` replaces `MINIO_TEST_BINARY` and defaults to `silo` on PATH.
