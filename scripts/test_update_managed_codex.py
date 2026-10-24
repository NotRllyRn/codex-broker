import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import io
import json

spec = importlib.util.spec_from_file_location("updater", Path(__file__).with_name("update-managed-codex.py"))
updater = importlib.util.module_from_spec(spec)
spec.loader.exec_module(updater)


class UpgradeTests(unittest.TestCase):
    def fixture(self, directory):
        root = Path(directory)
        for name in updater.PINS:
            target = root / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text((updater.ROOT / name).read_text())
        return root

    def test_upgrade_and_repeat(self):
        with tempfile.TemporaryDirectory() as directory:
            root = self.fixture(directory)
            updater.synchronize(root, "1.2.3")
            updater.synchronize(root, "1.2.3", check=True)
            self.assertEqual((root / "managed-codex-version.txt").read_text(), "1.2.3\n")
            with self.assertRaisesRegex(ValueError, "disagree"):
                updater.synchronize(root, "1.2.4", check=True)

    def test_missing_pin_changes_nothing(self):
        with tempfile.TemporaryDirectory() as directory:
            root = self.fixture(directory)
            (root / "README.md").write_text("pin removed")
            before = (root / "Dockerfile").read_text()
            with self.assertRaisesRegex(ValueError, "expected"):
                updater.synchronize(root, "1.2.3")
            self.assertEqual((root / "Dockerfile").read_text(), before)
            self.assertFalse((root / "managed-codex-version.txt").exists())

    def test_latest_release_and_noop_outputs(self):
        for current, candidate, changed in (("1.2.3", "1.2.4", True), ("1.2.3", "1.2.3", False), ("1.2.3", "1.2.2", False)):
            with self.subTest(candidate=candidate), tempfile.TemporaryDirectory() as directory:
                root = self.fixture(directory)
                updater.synchronize(root, current)
                output = root / "outputs"
                metadata = io.BytesIO(json.dumps({"name": "@openai/codex", "version": candidate}).encode())
                with patch.object(updater, "ROOT", root), patch("sys.argv", ["updater", "--latest"]), patch.dict("os.environ", {"GITHUB_OUTPUT": str(output)}), patch.object(updater.urllib.request, "urlopen", return_value=metadata):
                    updater.main()
                self.assertIn(f"changed={str(changed).lower()}", output.read_text())
                expected = candidate if changed else current
                updater.synchronize(root, expected, check=True)
                self.assertEqual((root / "managed-codex-version.txt").read_text().strip(), expected)

    def test_version_validation_and_numeric_order(self):
        for value in (None, "latest", "1.2.3-beta.1", "1.2.3\n", "01.2.3", "1.2.3;echo bad"):
            with self.assertRaises(ValueError):
                updater.parse_version(value)
        self.assertGreater(updater.parse_version("0.100.0"), updater.parse_version("0.99.0"))


if __name__ == "__main__":
    unittest.main()
