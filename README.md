# snapzner

`snapzner` is a one-shot command-line tool for creating, retaining, deleting,
and replaying Hetzner Cloud server snapshots across multiple projects.

It creates live snapshots of servers selected by Hetzner labels or explicit
server references. Each configured project has a separate API token and is
processed independently with bounded concurrency.

## Features

- Multiple Hetzner Cloud projects in one invocation.
- Label-selector and explicit include/exclude server selection.
- Per-server bounded retention with latest and age-target recovery points.
- Reversible snapshot pins that no prune mode can override.
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

- The global server label selector, bounded age-target retention, and
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
  retention_label: "AUTOBACKUP.KEEP-MAX"
  keep_max: 5
  keep_latest: 2
  keep_targets: [1d, 1w, 2w]
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
AUTOBACKUP.KEEP-MAX=7
```

The value must be an integer of at least one and overrides `keep_max` for that
server. Snapzner records the effective maximum on each new snapshot so later
standalone prune operations use the same override.

Retention is evaluated separately for each source server, with snapshots
ordered newest first. Snapzner retains the first `keep_latest` snapshots. It
then processes `keep_targets` from youngest to oldest. For each target, it
retains the newest snapshot that:

- is at least as old as the target; and
- has not already been retained by another slot.

Every other managed snapshot is eligible for pruning. Processing stops when
the retained count reaches `keep_max`. If no snapshot is old enough for a
target, that slot remains empty; Snapzner does not substitute a newer snapshot.
The configured `keep_latest` plus the number of targets cannot exceed
`keep_max`.

For example, with five snapshots per day, `keep_max: 5`, `keep_latest: 2`, and
`keep_targets: [1d, 1w, 2w]` retains the latest two snapshots plus one snapshot
at least 24 hours old, one at least 168 hours old, and one at least 336 hours
old. Each target chooses the newest qualifying snapshot, giving five distinct
recovery points when sufficient history exists.

Targets are fixed elapsed durations relative to pruning time, not calendar
dates. In addition to Go duration units, Snapzner accepts `d` as 24 hours and
`w` as 168 hours, including composites such as `1w2d12h`. Targets must be
positive, unique, and ordered from youngest to oldest. An empty list disables
age targets.

Deletion-protected snapshots are retained unless pruning uses `--force`.
Consequently, protected snapshots can make the actual snapshot count exceed
`keep_max`.

Snapshots carrying `snapzner.mlahr.dev/pinned=v1` are excluded before
retention is evaluated. Pins do not consume retention slots, cannot be
overridden by `prune --force`, and can therefore also make the actual snapshot
count exceed `keep_max`.

Configurations containing the former `keep_min`, `keep_last`, `min_age`, and
`max_age` fields remain loadable. Because that policy has no exact age-target
equivalent, a legacy-only configuration uses the new default retention policy;
any new retention fields already present take precedence. Running `snapzner
configure` and saving rewrites the file without the legacy fields. The former
default label `AUTOBACKUP.KEEP-LAST` is likewise migrated to
`AUTOBACKUP.KEEP-MAX`; custom retention-label names are preserved. Commands
that consume project configuration emit a migration warning until the file is
rewritten without the legacy fields; `configure` itself does not emit that
warning.

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

Back up managed servers by ID without knowing their projects:

```sh
snapzner backup --server 123456 --server id:789012
```

When every unqualified `--server` value is a numeric ID and no `--project` is
given, Snapzner searches the effective managed selection of every configured
project. It groups matches by project before creating snapshots. IDs that are
not selected by any configured project are reported and skipped successfully.
Discovery or credential failures, and an ID matching more than one project,
fail the complete preflight before any snapshot is created.

For multiple projects, qualify each server with its project alias. When every
server is qualified, the project list is derived from the values:

```sh
snapzner backup \
  --server production/database \
  --server staging/name:web \
  --server staging/id:123456
```

`--server` is repeatable and accepts a name, numeric ID, `name:VALUE`, or
`id:VALUE`. Unqualified names require exactly one `--project`; unqualified IDs
without a project use the cross-project discovery behavior described above.
When qualified values and `--project` are combined, their project sets must
match exactly. The filter is per-run and does not modify configuration. Without
`--force`, every requested project-scoped server must already belong to the
project's effective configured selection, including its explicit exclusions.
Snapzner validates all requested servers across all projects before creating
any snapshot.

Pass `--force` to back up explicitly requested, project-scoped servers even
when they do not belong to the project's effective configured selection:

```sh
snapzner backup --project staging --server api-staging --force
```

Forced targets still resolve through the selected project's Hetzner
credential. `--force` requires at least one `--server`; an unqualified numeric
server ID also requires `--project` because cross-project discovery considers
only configured selection.

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
pruned. Pinned snapshots are always reported and retained, including with
`--force`. Deletion-protected snapshots are reported and retained unless
pruning uses `snapzner prune --apply --force`; Snapzner then disables their
deletion protection before deleting them.

List managed snapshots, pin or unpin exact IDs, or delete exact IDs:

```sh
snapzner snapshots list --project production
snapzner snapshots pin --project production --id 123
snapzner snapshots unpin --project production --id 123
snapzner snapshots delete --project production --id 123 --id 456
```

Use `snapzner snapshots list --all --project production` to include snapshots
not managed by Snapzner. List output identifies each snapshot as managed or
unmanaged and pinned or unpinned, and includes its image ID. Pinning adds only
Snapzner's metadata label; it does not enable Hetzner deletion protection or
make an unmanaged snapshot managed.

Deleting an unmanaged or deletion-protected snapshot requires both an exact ID
and `--force`. For a protected snapshot, Snapzner disables deletion protection
before deletion. A pinned snapshot cannot be deleted even with `--force`; it
must be unpinned first. Backup, system, and app images are never deleted by
this command. Interactive confirmation is required unless `--yes` is supplied.

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
