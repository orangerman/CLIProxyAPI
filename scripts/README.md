# Local systemd deployment

Run the script as the user owning `cliproxyapi.service`, not as root. It deploys
the current backend checkout and its sibling `Cli-Proxy-API-Management-Center`
checkout. It does not fetch, merge, or reset either repository.

```sh
python3 scripts/deploy-local.py --check
python3 scripts/deploy-local.py
```

Requirements: Python 3.11+, Bun, Go, Git, an existing v8 HTTP service managed by
systemd's user manager, and the existing `~/.config/cliproxyapi/credentials.json`
with its `management_key`. Secrets are read locally and never passed in command
arguments or printed. See `--help` for directory and service overrides.

The script builds both artifacts and runs frontend verification plus backend
usage/API tests before stopping the service. It then backs up and verifies the
complete configuration and data directories, external auth directory if needed,
and systemd unit. Backups are private and remain in
`~/.local/state/cliproxyapi/deployments/<timestamp>/`. They contain credentials;
keep them private. Build logs are saved alongside each backup.

Deployment replaces only the server binary, management HTML, version marker,
and configuration. Existing configuration values are retained, except that
`management.disable-auto-update-panel` and
`observability.usage.usage-statistics-enabled` are set to `true`. YAML formatting
and comments are rewritten. Credentials, plugins, logs, and `.usage-history`
remain in their existing locations. No sample records are installed.

Health checks verify authentication, loaded credential names, the exact panel
artifact, and enabled usage summaries. A failure after shutdown automatically
restores the previous artifacts and configuration and restarts the old service.
The service is briefly unavailable during backup and replacement.

To manually undo a successful deployment:

```sh
python3 scripts/deploy-local.py --rollback ~/.local/state/cliproxyapi/deployments/<timestamp>
```

Rollback restores the previous binary, panel, version, and configuration (so any
subsequent configuration edits would be replaced). It never restores the full
data archive over live credentials or usage history. Keep the printed backup
directory for recovery. Deployments are serialized using a file lock.

Historical token usage from a version that did not persist it cannot be
reconstructed. The old transient usage queue and in-memory request counters do
not survive a process restart; credential and API-key counter snapshots are
archived for inspection, not imported into the new journal. New usage records
are persisted beside the config in `.usage-history/` and survive later deployments.
The script does not stop Redis or other services and rejects external storage or
static-path overrides that require a different backup plan.
