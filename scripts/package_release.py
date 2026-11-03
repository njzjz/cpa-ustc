"""Build store-compatible, root-library ZIPs and separate companion CLI ZIPs.

Only native, architecture-checked artifacts are accepted. Publishing requires
the complete five-platform set; partial releases must never become latest.
"""

import argparse
import hashlib
from pathlib import Path
import re
import subprocess
import zipfile

TARGETS = {("darwin", "amd64"), ("darwin", "arm64"), ("linux", "amd64"), ("linux", "arm64"), ("windows", "amd64")}


def library_name(goos):
    return "ustc." + {"darwin": "dylib", "linux": "so", "windows": "dll"}[goos]


def check_binary(data, goos, goarch):
    """Reject a mislabeled release even when the ZIP/filename looks correct."""
    if goos == "linux":
        valid = data[:6] == b"\x7fELF\x02\x01" and int.from_bytes(data[18:20], "little") == {"amd64": 62, "arm64": 183}[goarch]
    elif goos == "darwin":
        valid = data[:4] == b"\xcf\xfa\xed\xfe" and int.from_bytes(data[4:8], "little") == {"amd64": 0x1000007, "arm64": 0x100000C}[goarch]
    else:
        offset = int.from_bytes(data[60:64], "little")
        valid = data[:2] == b"MZ" and offset >= 64 and data[offset:offset + 6] == b"PE\0\0\x64\x86"
    if not valid:
        raise ValueError(f"invalid native binary for {goos}/{goarch}")


def package(directory, version, goos, goarch):
    if (goos, goarch) not in TARGETS:
        raise ValueError("unsupported release target")
    executable = "cpa-ustc.exe" if goos == "windows" else "cpa-ustc"
    reported = subprocess.check_output([str((directory / executable).resolve()), "version"], text=True).strip()
    if reported != f"cpa-ustc {version}":
        raise ValueError("release tag does not match compiled version")
    for prefix, filename in (("ustc", library_name(goos)), ("cpa-ustc", executable)):
        binary = (directory / filename).read_bytes()
        check_binary(binary, goos, goarch)
        archive = directory / f"{prefix}_{version}_{goos}_{goarch}.zip"
        with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as output:
            info = zipfile.ZipInfo(filename)
            info.create_system = 3
            info.external_attr = (0o100755 << 16)
            output.writestr(info, binary, compress_type=zipfile.ZIP_DEFLATED)


def verify_all(directory, version):
    expected = {f"{prefix}_{version}_{goos}_{goarch}.zip": (prefix, goos, goarch) for prefix in ("ustc", "cpa-ustc") for goos, goarch in TARGETS}
    actual = {file.name for file in directory.glob("*.zip")}
    if actual != expected.keys():
        raise ValueError(f"incomplete release: missing {expected.keys() - actual}, unexpected {actual - expected.keys()}")
    checksums = []
    for name, (prefix, goos, goarch) in sorted(expected.items()):
        archive = directory / name
        with zipfile.ZipFile(archive) as source:
            filename = library_name(goos) if prefix == "ustc" else "cpa-ustc.exe" if goos == "windows" else "cpa-ustc"
            if source.namelist() != [filename] or source.testzip() is not None:
                raise ValueError(f"invalid archive layout or CRC: {name}")
            check_binary(source.read(filename), goos, goarch)
        checksums.append(f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {name}\n")
    (directory / "checksums.txt").write_text("".join(checksums), encoding="utf-8")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--goos")
    parser.add_argument("--goarch")
    parser.add_argument("--directory", type=Path, default=Path("dist"))
    parser.add_argument("--verify-all", action="store_true")
    args = parser.parse_args()
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", args.tag):
        parser.error("tag must be v<major>.<minor>.<patch>")
    if args.verify_all:
        verify_all(args.directory, args.tag[1:])
    else:
        if not args.goos or not args.goarch:
            parser.error("--goos and --goarch are required for packaging")
        package(args.directory, args.tag[1:], args.goos, args.goarch)


if __name__ == "__main__":
    main()
