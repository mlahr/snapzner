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

## Configure projects

Run configuration as the same Unix user that will run the cron job:

```sh
snapzner configure --project production
snapzner configure --project staging
snapzner projects list
```

`configure` prompts for the API token without echoing it, verifies access, and
stores it in `credentials.yaml`. The configuration directory is mode `0700` and
both YAML files are mode `0600`.

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
  keep_last: 3
  snapshot_name: "%name%-%timestamp%"
  operation_timeout: 1h
  project_concurrency: 4
  server_concurrency: 4

projects:
  - name: production
    include: ["id:123456", "name:database"]
    exclude: ["name:temporary"]
```

Each project may override `label_selector`, `retention_label`, `keep_last`, and
`snapshot_name`. Set a project's `label_selector` to an empty string to use only
its explicit include list.

Effective selection is:

```text
(label selector matches union explicit includes) minus explicit excludes
```

Explicit references must use `id:` or `name:`. Unresolved references fail that
project before any snapshots are created.

A selected server can override retention with the configured retention label:

```text
AUTOBACKUP.KEEP-LAST=7
```

The value must be an integer of at least one.

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

After a server's new snapshot becomes available, `backup` enforces that
server's retention. A failed snapshot never triggers automatic pruning for
that server.

Preview standalone pruning, then apply it:

```sh
snapzner prune
snapzner prune --apply
```

Only snapshots carrying `snapzner.mlahr.dev/managed=v1` are automatically
pruned. Deletion-protected snapshots are reported and retained.

List managed snapshots or delete exact IDs:

```sh
snapzner snapshots list --project production
snapzner snapshots delete --project production --id 123 --id 456
```

Deleting an unmanaged snapshot requires both an exact ID and
`--force-unmanaged`. Backup, system, and app images are never deleted by this
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
current attachments, and it respects Hetzner rebuild protection. Snapzner does
not disable protection automatically.

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
- `--quiet` suppresses successful human-readable events.
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
