"""Offline release-gate tests; these synthetic headers are never published."""

import importlib.util
from pathlib import Path
import tempfile
import unittest
import zipfile

spec = importlib.util.spec_from_file_location("package_release", Path(__file__).resolve().parents[1] / "scripts/package_release.py")
packager = importlib.util.module_from_spec(spec)
spec.loader.exec_module(packager)


def header(goos, goarch):
    data = bytearray(128)
    if goos == "linux":
        data[:6] = b"\x7fELF\x02\x01"
        data[18:20] = {"amd64": 62, "arm64": 183}[goarch].to_bytes(2, "little")
    elif goos == "darwin":
        data[:4] = b"\xcf\xfa\xed\xfe"
        data[4:8] = {"amd64": 0x1000007, "arm64": 0x100000C}[goarch].to_bytes(4, "little")
    else:
        data[:2] = b"MZ"
        data[60:64] = (64).to_bytes(4, "little")
        data[64:70] = b"PE\0\0\x64\x86"
    return data


class ReleaseGate(unittest.TestCase):
    def test_binary_architecture_checks(self):
        for goos, goarch in packager.TARGETS:
            packager.check_binary(header(goos, goarch), goos, goarch)
            with self.assertRaises(ValueError):
                packager.check_binary(b"not a library", goos, goarch)
        with self.assertRaises(ValueError):
            packager.check_binary(header("linux", "amd64"), "linux", "arm64")

    def test_all_five_platforms_required_and_root_layout_checked(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            with self.assertRaises(ValueError):
                packager.verify_all(directory, "1.2.3")
            for goos, goarch in packager.TARGETS:
                for prefix in ("ustc", "cpa-ustc"):
                    name = packager.library_name(goos) if prefix == "ustc" else "cpa-ustc.exe" if goos == "windows" else "cpa-ustc"
                    with zipfile.ZipFile(directory / f"{prefix}_1.2.3_{goos}_{goarch}.zip", "w") as archive:
                        archive.writestr(name, header(goos, goarch))
            packager.verify_all(directory, "1.2.3")
            self.assertEqual(len((directory / "checksums.txt").read_text().splitlines()), 10)
            archive_path = directory / "ustc_1.2.3_windows_amd64.zip"
            with zipfile.ZipFile(archive_path, "w") as archive:
                archive.writestr("nested/ustc.dll", header("windows", "amd64"))
            with self.assertRaises(ValueError):
                packager.verify_all(directory, "1.2.3")


if __name__ == "__main__":
    unittest.main()
