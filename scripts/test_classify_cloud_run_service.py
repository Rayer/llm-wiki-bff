import subprocess
from pathlib import Path
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "classify_cloud_run_service.py"
SERVICE_NAME = "llm-wiki-auth"


class CloudRunServiceAbsenceClassificationTest(unittest.TestCase):
    def run_classifier(self, stderr_text):
        with tempfile.NamedTemporaryFile("w+", encoding="utf-8", delete=False) as handle:
            handle.write(stderr_text)
            path = Path(handle.name)
        try:
            return subprocess.run(
                ["python3", str(SCRIPT), "--service-name", SERVICE_NAME, "--stderr-file", str(path)],
                capture_output=True,
                text=True,
            )
        finally:
            path.unlink(missing_ok=True)

    def test_exact_cloud_run_cannot_find_service_is_absent(self):
        result = self.run_classifier(
            "ERROR: (gcloud.run.services.describe) Cannot find service [llm-wiki-auth]\n"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "")
        self.assertEqual(result.stderr, "")

    def test_broader_not_found_patterns_are_not_treated_as_absent(self):
        result = self.run_classifier("ERROR: not found\n")
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertEqual(result.stdout, "")
        self.assertEqual(result.stderr, "")


if __name__ == "__main__":
    unittest.main()
