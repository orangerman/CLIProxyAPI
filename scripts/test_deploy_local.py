"""Safety checks for local deployment; never access the real service."""

import importlib.util
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("deploy_local", Path(__file__).with_name("deploy-local.py"))
deploy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deploy)


class DeploymentTests(unittest.TestCase):
    def test_config_preserves_credentials_and_unrelated_settings(self):
        config = {"config-version": 8, "management": {"secret-key": "test-hash", "allow-remote": True},
                  "access": {"api-keys": ["test-only"]}, "oauth": {"auth-dir": "/example/auths"},
                  "observability": {"logs": {"debug": False}, "usage": {"redis-usage-queue-retention-seconds": 60}}}
        original = json.loads(json.dumps(config))
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "config.yaml"
            deploy.stage_config(config, target)
            actual = deploy.parse_config(target)
            self.assertTrue(actual["management"].pop("disable-auto-update-panel"))
            self.assertTrue(actual["observability"]["usage"].pop("usage-statistics-enabled"))
            self.assertEqual(actual, original)
            self.assertEqual(config, original)
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)

    def test_failed_health_check_rolls_back_without_replacing_data(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            data, config_dir, frontend, backups = (root / name for name in ("data", "config", "frontend", "backups"))
            for path in (data / "auths", config_dir / "static", config_dir / ".usage-history", frontend / "dist", backups):
                path.mkdir(parents=True)
            files = {data / "cli-proxy-api": "old binary", data / "VERSION": "old version",
                     config_dir / "config.yaml": "old config", config_dir / "static/management.html": "old panel",
                     data / "auths/account.json": "live credential", config_dir / ".usage-history/day.jsonl": "live history"}
            for path, value in files.items():
                path.write_text(value)
            (frontend / "dist/index.html").write_text("new panel")
            unit = root / "service"
            unit.write_text("test unit")
            args = SimpleNamespace(data_dir=data, config_dir=config_dir, frontend=frontend,
                                   backup_root=backups, service="test.service", check=False)
            credentials = {"files": [{"name": "account.json"}]}
            service_calls = []

            def fake_systemctl(*command):
                service_calls.append(command)
                return str(unit) if command[0] == "show" else ""

            def fake_run(command, **_kwargs):
                if command[:2] == ["go", "build"]:
                    Path(command[command.index("-o") + 1]).write_text("new binary")
                return SimpleNamespace(stdout="1234567890abcdef\n")

            with patch.object(deploy, "prepare", return_value=({}, data / "auths", "test-key", "http://test", credentials)), \
                    patch.object(deploy, "run", side_effect=fake_run), \
                    patch.object(deploy, "stage_config", side_effect=lambda _c, p: p.write_text("new config")), \
                    patch.object(deploy, "api", return_value=credentials), \
                    patch.object(deploy, "systemctl", side_effect=fake_systemctl), \
                    patch.object(deploy, "wait_healthy", side_effect=[RuntimeError("simulated failure"), None]):
                with self.assertRaisesRegex(RuntimeError, "simulated failure"):
                    deploy.deploy(args)
            for path, value in files.items():
                self.assertEqual(path.read_text(), value, str(path))
            self.assertEqual(service_calls.count(("start", "test.service")), 2)
            backup = next(backups.iterdir())
            self.assertTrue((backup / "data.tar.gz").is_file())
            self.assertFalse((backup / "SUCCESS").exists())


if __name__ == "__main__":
    unittest.main()
