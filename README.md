# snapzner

`snapzner` is a one-shot command-line tool for creating, retaining, deleting,
and replaying Hetzner Cloud server snapshots across multiple projects.

It creates live snapshots of servers selected by Hetzner labels or explicit
server references. Each configured project has a separate API token and is
processed independently with bounded concurrency.

## Features

- Multiple Hetzner Cloud projects in one invocation.
- Label-selector and explicit include/exclude server selection.
- Per-project and per-server count-based retention.
- Managed-snapshot ownership labels that prevent accidental pruning of manual snapshots.
- Previewable standalone pruning and exact-ID deletion.
- Clone replay to a newly billed server.
- Destructive in-place rebuild from a snapshot.
- Human-readable and JSON output for cron and CI use.
- Owner-only local credential storage for unattended user cron.

## Installation

On Debian-based Linux `amd64` or `arm64`, install the latest release:

```sh
curl -fsSL https://raw.githubusercontent.com/mlahr/snapzner/main/install.sh | bash
```

The installer downloads the matching `.deb` from the latest GitHub Release,
verifies it against `checksums.txt`, and installs it with `apt-get`.

Alternatively, download a `.deb` or tarball from
[GitHub Releases](https://github.com/mlahr/snapzner/releases), or build from source:

```sh
go build -o snapzner .
```

## Configure Snapzner

Run configuration as the same Unix user that will run the cron job:

```sh
snapzner configure
snapzner projects list
```

The interactive wizard configures:

- The global server label selector, count- and age-based retention, and
  snapshot naming format.
- Operation timeout and project/server concurrency.
- Every Hetzner project and its API token.
- The exact final set of servers to back up in each project.

After validating a project's token, Snapzner opens a full-screen server picker.
Use the arrow keys to move, Space to toggle, `a` to select all, `n` to select
none, Enter to confirm, or Esc to cancel. Snapzner converts that
final selection into stable ID-based `include` and `exclude` entries; these
lists do not need to be maintained manually.

Running `configure` again loads the current settings, lets you retain or remove
each project, and offers to add more projects. Existing tokens remain hidden
and are kept unless you request replacement or validation fails. Nothing is
written until you confirm the final summary.

Tokens are prompted without echoing and stored in `credentials.yaml`. The
configuration directory is mode `0700` and both YAML files are mode `0600`.

On Linux, the default paths are:

```text
~/.config/snapzner/config.yaml
~/.config/snapzner/credentials.yaml
```

This is access control through Unix ownership and permissions, not encryption
at rest. Root and processes running as that Unix user can read the tokens.

Use `--config /path/to/config.yaml` to select another configuration. Its
credential file is stored beside it.

## Configuration

See [`config.example.yaml`](config.example.yaml) for a complete starting point.

```yaml
version: 1

defaults:
  label_selector: "AUTOBACKUP=true"
  retention_label: "AUTOBACKUP.KEEP-LAST"
  keep_min: 1
  keep_last: 3
  min_age: 24h
  max_age: 30d
  snapshot_name: "%name%-%timestamp%"
  operation_timeout: 1h
  project_concurrency: 4
  server_concurrency: 4

projects:
  - name: production
    include: ["id:123456", "name:database"]
    exclude: ["name:temporary"]
```

All projects use the global selector, retention, and naming settings. Enter `-`
for the label selector in `configure` to disable label selection and use only
the servers selected through the picker.

Effective selection is:

```text
(label selector matches union explicit includes) minus explicit excludes
```

The wizard writes stable `id:` references. If an existing reference no longer
resolves, the next configuration run reports and removes it.

A selected server can override retention with the configured retention label:

```text
AUTOBACKUP.KEEP-LAST=7
```

The value must be an integer of at least one.

Retention is evaluated separately for each source server, with snapshots
ordered newest first. Snapzner always retains `keep_min` snapshots. For every
remaining snapshot, it deletes the snapshot when either:

```text
age >= max_age
or
position is outside keep_last and age >= min_age
```

An omitted or zero age disables that age bound. With a disabled `min_age`,
snapshots outside `keep_last` are immediately eligible, preserving count-only
retention. With a disabled `max_age`, age never overrides `keep_last`.

For example, `keep_min: 1`, `keep_last: 3`, `min_age: 24h`, and `max_age: 30d`
always retains the newest snapshot, retains snapshots two and three for at
most 30 days, and deletes additional snapshots once they are at least 24 hours
old. A per-server retention-label value overrides only `keep_last`; values
below `keep_min` are clamped to `keep_min`.

Age values are fixed elapsed durations. In addition to Go duration units,
Snapzner accepts `d` as 24 hours and `w` as 168 hours, including composites
such as `1w2d12h`.

Snapshot names support `%project%`, `%id%`, `%name%`, `%timestamp%`, `%date%`,
and `%time%`. Date and time placeholders use UTC.

## Back up and prune

Back up every configured project:

```sh
snapzner backup
```

Back up selected projects:

```sh
snapzner backup --project production --project staging
```

Back up specific configured servers in one project by name or numeric ID:

```sh
snapzner backup --project production \
  --server database \
  --server 123456
```

For multiple projects, qualify each server with its project alias. When every
server is qualified, the project list is derived from the values:

```sh
snapzner backup \
  --server production/database \
  --server staging/name:web \
  --server staging/id:123456
```

`--server` is repeatable and accepts a name, numeric ID, `name:VALUE`, or
`id:VALUE`. Unqualified values require exactly one `--project`. When qualified
values and `--project` are combined, their project sets must match exactly. The
filter is per-run and does not modify configuration. Every requested server
must already belong to the project's effective configured selection; the flag
cannot override an explicit exclusion. Snapzner validates all requested
servers across all projects before creating any snapshot.

After a server's new snapshot becomes available, `backup` enforces that
server's retention. With `--server`, automatic retention is likewise limited
to requested servers whose snapshots succeeded. A failed snapshot never
triggers automatic pruning for that server. While a backup is running,
Snapzner reports server selection,
snapshot creation, completion counts, and retention progress on standard
error. Snapshot creation uses an animated spinner when standard error is a
terminal and plain progress lines when it is redirected. Final human-readable
or JSON results remain on standard output.

Preview standalone pruning, then apply it:

```sh
snapzner prune
snapzner prune --apply
```

Only snapshots carrying `snapzner.mlahr.dev/managed=v1` are automatically
pruned. Deletion-protected snapshots are reported and retained. To include
them deliberately, use `snapzner prune --apply --force`; Snapzner disables
their deletion protection before deleting them.

List managed snapshots or delete exact IDs:

```sh
snapzner snapshots list --project production
snapzner snapshots delete --project production --id 123 --id 456
```

Use `snapzner snapshots list --all --project production` to include snapshots
not managed by Snapzner. List output identifies each snapshot as managed or
unmanaged and includes its image ID.

Deleting an unmanaged or deletion-protected snapshot requires both an exact ID
and `--force`. For a protected snapshot, Snapzner disables deletion protection
before deletion. Backup, system, and app images are never deleted by this
command. Interactive confirmation is required unless `--yes` is supplied.

## Replay snapshots

Create a new, billed server from a managed snapshot:

```sh
snapzner replay clone --project production --snapshot latest --source database
snapzner replay clone --project production --snapshot 123456 --name database-test
```

Clone replay restores the recorded server type, location, public-network
enablement, user labels, and directly attached firewalls. It allocates new
public IPs and does not attach Volumes, private networks, placement groups,
Floating IPs, or existing Primary IP resources. The backup-selection label is
removed from the clone.

For an unmanaged snapshot without Snapzner metadata, provide at least
`--server-type` and `--location`. `--ipv4` and `--ipv6` accept `true` or `false`.

Overwrite an existing server's root disk in place:

```sh
snapzner replay rebuild --project production \
  --snapshot latest --source database --target database
```

Rebuild is destructive. It retains the target Hetzner server resource and its
current attachments. If the target has rebuild protection, `--force` is
required; Snapzner temporarily disables the protection and restores it after
the rebuild action finishes.

Clone and rebuild commands require interactive confirmation, or `--yes` for
deliberate non-interactive use.

## Cron

Snapzner contains no scheduler. Example user crontab:

```cron
0 1 * * * /usr/bin/snapzner backup --quiet >>"$HOME/.local/state/snapzner.log" 2>&1
```

Create the log directory first and ensure cron has the same `HOME` as the user
who ran `configure`. Mutating commands use a non-blocking local lock, so an
overlapping run fails instead of creating duplicate concurrent runs.

## Output and failure behavior

- `--json` emits a JSON array of result events.
- `--quiet` suppresses successful human-readable events and backup progress.
- Independent projects and servers continue after failures.
- The process exits nonzero if any requested operation fails.
- `SIGINT` and `SIGTERM` cancel waits and pending API requests. An already
  accepted Hetzner action may continue remotely.

## Snapshot consistency and scope

Snapzner always snapshots running servers without quiescing or shutting them
down. The resulting disk image is crash-consistent; application-level
consistency is the operator's responsibility.

Hetzner server snapshots contain the root disk. They are not backups of
separately attached Hetzner Volumes.

Snapshots created by other tools, including the predecessor tool's
`AUTOBACKUP` images, are not automatically adopted or pruned.

## License

MIT
