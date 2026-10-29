#!/usr/bin/env python3
"""Build and deploy this checkout to the existing local systemd user service."""

import argparse
import datetime as dt
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request


REPO = Path(__file__).resolve().parents[1]


def run(args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def systemctl(*args):
    return run(["systemctl", "--user", *args], capture_output=True, text=True).stdout.strip()


def digest(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def atomic_copy(source, target):
    target = Path(target)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=".deploy-", dir=target.parent)
    os.close(fd)
    try:
        shutil.copy2(source, temporary)
        os.replace(temporary, target)
    finally:
        Path(temporary).unlink(missing_ok=True)


def api(base, key, route):
    request = urllib.request.Request(base + "/v8/management/" + route,
                                     headers={"Authorization": "Bearer " + key})
    # Ignore shell proxy settings for local health checks.
    with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(request, timeout=5) as response:
        return json.load(response)


def credential_names(response):
    return sorted(item["name"] for item in response.get("files", []))


def wait_healthy(service, base, key, panel_hash, expected_names, summary=True):
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline:
        try:
            if systemctl("is-active", service) != "active":
                raise RuntimeError("service not active")
            names = credential_names(api(base, key, "credentials"))
            if not set(expected_names).issubset(names):
                raise RuntimeError("previous credentials not loaded")
            with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(
                    base + "/management.html", timeout=5) as response:
                if hashlib.sha256(response.read()).hexdigest() != panel_hash:
                    raise RuntimeError("served panel differs from installed artifact")
            if summary:
                today = dt.datetime.now(dt.timezone.utc).date()
                value = api(base, key, "observability/usage/summary?from=" + str(today)
                            + "T00:00:00Z&to=" + str(today + dt.timedelta(days=1)) + "T00:00:00Z")
                if value.get("recording_enabled") is not True:
                    raise RuntimeError("usage recording not enabled")
            return
        except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError):
            time.sleep(1)
    raise RuntimeError("Service health checks failed; see the systemd journal locally")


def restore(backup, manifest):
    """Restore only deployment artifacts; never replace credentials or usage history."""
    systemctl("stop", manifest["service"])
    for name, target in manifest["targets"].items():
        old = backup / "previous" / name
        if old.exists():
            atomic_copy(old, target)
        else:
            Path(target).unlink(missing_ok=True)
    systemctl("start", manifest["service"])


def parse_config(path):
    result = run(["bun", "-e", "console.log(JSON.stringify(Bun.YAML.parse(await Bun.stdin.text())))"],
                 input=path.read_text(), text=True, capture_output=True)
    return json.loads(result.stdout)


def stage_config(config, destination):
    # Preserve every existing setting, changing only these two deployment requirements.
    updated = json.loads(json.dumps(config))
    updated.setdefault("management", {})["disable-auto-update-panel"] = True
    updated.setdefault("observability", {}).setdefault("usage", {})["usage-statistics-enabled"] = True
    result = run(["bun", "-e", "console.log(Bun.YAML.stringify(JSON.parse(await Bun.stdin.text())))"],
                 input=json.dumps(updated), text=True, capture_output=True)
    destination.write_text(result.stdout)
    destination.chmod(0o600)
    if parse_config(destination) != updated:
        raise RuntimeError("Config round-trip validation failed")


def prepare(args):
    config_path = args.config_dir / "config.yaml"
    config = parse_config(config_path)
    if config.get("config-version") != 8:
        raise RuntimeError("This script requires an existing v8 configuration")
    if config.get("server", {}).get("tls", {}).get("enable"):
        raise RuntimeError("This local deployment script requires an HTTP listener")
    if config.get("management", {}).get("disable-control-panel"):
        raise RuntimeError("The management panel is disabled")
    executable = args.data_dir / "cli-proxy-api"
    command = systemctl("show", args.service, "--property=ExecStart", "--value")
    if f"path={executable} ;" not in command or f"--config {config_path} ;" not in command:
        raise RuntimeError("Service paths do not match --data-dir and --config-dir")
    if systemctl("show", args.service, "--property=WorkingDirectory", "--value") != str(args.data_dir):
        raise RuntimeError("Unexpected service working directory")
    pid = int(systemctl("show", args.service, "--property=MainPID", "--value"))
    if pid <= 0:
        raise RuntimeError("The existing service must be running for deployment preflight")
    environment = dict(item.split(b"=", 1) for item in Path(f"/proc/{pid}/environ").read_bytes().split(b"\0") if b"=" in item)
    for key, value in environment.items():
        if value and (key.upper().startswith((b"PGSTORE", b"GITSTORE", b"OBJECTSTORE"))
                      or key.upper() in (b"WRITABLE_PATH", b"MANAGEMENT_STATIC_PATH")):
            raise RuntimeError("External storage/static overrides need a deployment-specific backup plan")
    auth_dir = Path(config["oauth"]["auth-dir"]).expanduser()
    if not auth_dir.is_absolute() or not auth_dir.is_dir():
        raise RuntimeError("An existing absolute oauth.auth-dir is required")
    key = json.loads((args.config_dir / "credentials.json").read_text())["management_key"]
    base = "http://127.0.0.1:" + str(config["server"]["port"])
    credentials = api(base, key, "credentials")
    print(f"Preflight OK: {len(credential_names(credentials))} loaded credentials; {base}", flush=True)
    return config, auth_dir, key, base, credentials


def deploy(args):
    config, auth_dir, key, base, credentials = prepare(args)
    if args.check:
        return
    stamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    backup = args.backup_root / stamp
    backup.mkdir(mode=0o700)
    stage = backup / "stage"
    stage.mkdir()
    targets = {"binary": str(args.data_dir / "cli-proxy-api"),
               "panel": str(args.config_dir / "static/management.html"),
               "config": str(args.config_dir / "config.yaml"),
               "version": str(args.data_dir / "VERSION")}
    manifest = {"service": args.service, "targets": targets, "base": base,
                "credentials_path": str(args.config_dir / "credentials.json"),
                "credential_names": credential_names(credentials)}
    print(f"Building and checking; logs and backup: {backup}", flush=True)
    with (backup / "build.log").open("w") as log:
        for command, cwd in [
            (["bun", "install", "--frozen-lockfile"], args.frontend),
            (["bun", "run", "verify"], args.frontend),
            (["go", "test", "./internal/usagehistory", "./internal/redisqueue",
              "./internal/api/handlers/management", "./internal/api"], REPO),
        ]:
            run(command, cwd=cwd, stdout=log, stderr=subprocess.STDOUT)
        commit = run(["git", "rev-parse", "HEAD"], cwd=REPO, capture_output=True, text=True).stdout.strip()
        version = "local-" + commit[:8]
        run(["go", "build", "-ldflags", f"-s -w -X main.Version={version} -X main.Commit={commit} -X main.BuildDate={stamp}",
             "-o", str(stage / "binary"), "./cmd/server"], cwd=REPO, stdout=log, stderr=subprocess.STDOUT)
    shutil.copy2(args.frontend / "dist/index.html", stage / "panel")
    (stage / "version").write_text(version + "\n")
    # Re-read after builds so configuration changes during the build are not lost.
    config, auth_dir, key, base, credentials = prepare(args)
    manifest["credential_names"] = credential_names(credentials)
    stage_config(config, stage / "config")
    for route, name in [("credentials", "credentials-snapshot.json"),
                        ("observability/usage/api-keys", "api-key-usage-snapshot.json")]:
        (backup / name).write_text(json.dumps(api(base, key, route)))
    manifest["old_panel_hash"] = digest(targets["panel"])
    manifest["new_panel_hash"] = digest(stage / "panel")
    (backup / "manifest.json").write_text(json.dumps(manifest, indent=2))
    # Everything above happens while the existing service remains available.
    stopped = False
    saved = False
    try:
        stopped = True
        systemctl("stop", args.service)
        previous = backup / "previous"
        previous.mkdir()
        for name, path in targets.items():
            if Path(path).exists():
                shutil.copy2(path, previous / name)
        with tarfile.open(backup / "data.tar.gz", "w:gz") as archive:
            archive.add(args.config_dir, arcname="config")
            archive.add(args.data_dir, arcname="data")
            if not auth_dir.is_relative_to(args.data_dir) and not auth_dir.is_relative_to(args.config_dir):
                archive.add(auth_dir, arcname="external-auth")
            unit = systemctl("show", args.service, "--property=FragmentPath", "--value")
            archive.add(unit, arcname="systemd.service")
        # Verify the archive contents against every live file before replacing anything.
        with tarfile.open(backup / "data.tar.gz") as archive:
            for member in archive.getmembers():
                if not member.isfile() or member.name == "systemd.service":
                    continue
                root, _, relative = member.name.partition("/")
                live = {"config": args.config_dir, "data": args.data_dir, "external-auth": auth_dir}[root] / relative
                with archive.extractfile(member) as stream:
                    if hashlib.file_digest(stream, "sha256").hexdigest() != digest(live):
                        raise RuntimeError("Backup verification failed")
        saved = True
        for name, target in targets.items():
            atomic_copy(stage / name, target)
        systemctl("start", args.service)
        wait_healthy(args.service, base, key, manifest["new_panel_hash"], manifest["credential_names"])
    except BaseException:
        if stopped:
            print("Deployment failed; restoring previous service artifacts.", file=sys.stderr, flush=True)
            if saved:
                restore(backup, manifest)
            else:
                systemctl("start", args.service)
            wait_healthy(args.service, base, key, manifest["old_panel_hash"], manifest["credential_names"], summary=False)
        raise
    (backup / "SUCCESS").write_text(dt.datetime.now(dt.timezone.utc).isoformat() + "\n")
    print(f"Deployed successfully: {base}/management.html#/dashboard\nBackup: {backup}", flush=True)


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--frontend", type=Path, default=REPO.parent / "Cli-Proxy-API-Management-Center")
    parser.add_argument("--config-dir", type=Path, default=Path.home() / ".config/cliproxyapi")
    parser.add_argument("--data-dir", type=Path, default=Path.home() / ".local/share/cliproxyapi")
    parser.add_argument("--backup-root", type=Path, default=Path.home() / ".local/state/cliproxyapi/deployments")
    parser.add_argument("--service", default="cliproxyapi.service")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--check", action="store_true", help="Check deployment prerequisites without building or restarting")
    mode.add_argument("--rollback", type=Path, help="Restore artifacts and config from a backup; keep current auth/history data")
    args = parser.parse_args()
    args.backup_root.mkdir(parents=True, exist_ok=True, mode=0o700)
    with (args.backup_root / ".lock").open("w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if args.rollback:
            backup = args.rollback.resolve()
            if not (backup / "SUCCESS").exists():
                raise RuntimeError("Manual rollback requires a successful deployment backup")
            manifest = json.loads((backup / "manifest.json").read_text())
            key = json.loads(Path(manifest["credentials_path"]).read_text())["management_key"]
            restore(backup, manifest)
            wait_healthy(manifest["service"], manifest["base"], key, manifest["old_panel_hash"],
                         manifest["credential_names"], summary=False)
            print("Rollback complete; credentials and usage history were retained.")
        else:
            deploy(args)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        # Do not expose HTTP response bodies or configuration/credential contents.
        print(f"Deployment stopped: {type(error).__name__}: {error}", file=sys.stderr)
        sys.exit(1)
