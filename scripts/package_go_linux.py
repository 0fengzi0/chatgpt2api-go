#!/usr/bin/env python3
"""Create deterministic Linux release archives from the dist directories."""

from __future__ import annotations

import gzip
import hashlib
import tarfile
from pathlib import Path


ARCHITECTURES = ("amd64",)
EXECUTABLE_NAMES = {"chatgpt2api", "cleanup-images.sh"}
SOURCE_DIRS = {
    "amd64": "release-image-gateway-amd64",
}


def archive_name(arch: str) -> str:
    return f"chatgpt2api-image-gateway-linux-{arch}.tar.gz"


def add_tree(tar: tarfile.TarFile, source: Path, archive_root: str) -> None:
    for path in sorted(source.rglob("*"), key=lambda item: item.as_posix()):
        relative = f"{archive_root}/{path.relative_to(source).as_posix()}"
        info = tar.gettarinfo(str(path), arcname=relative)
        info.uid = 0
        info.gid = 0
        info.uname = ""
        info.gname = ""
        info.mtime = int(path.stat().st_mtime)
        if path.is_dir():
            if not any(child.is_file() for child in path.rglob("*")):
                continue
            info.mode = 0o755
            tar.addfile(info)
            continue
        info.mode = 0o755 if path.name in EXECUTABLE_NAMES else 0o644
        with path.open("rb") as handle:
            tar.addfile(info, handle)


def build_archive(dist: Path, arch: str) -> Path:
    source = dist / SOURCE_DIRS[arch]
    if not source.is_dir():
        raise FileNotFoundError(source)
    destination = dist / archive_name(arch)
    with destination.open("wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w") as tar:
                add_tree(tar, source, f"chatgpt2api-image-gateway-linux-{arch}")
    return destination


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    dist = root / "dist"
    archives = [build_archive(dist, arch) for arch in ARCHITECTURES]
    checksums = "".join(f"{sha256(path)}  {path.name}\n" for path in archives)
    (dist / "SHA256SUMS.txt").write_text(checksums, encoding="ascii")
    for path in archives:
        print(f"{path.name} {path.stat().st_size}")


if __name__ == "__main__":
    main()
