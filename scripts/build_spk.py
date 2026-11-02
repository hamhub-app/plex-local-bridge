#!/usr/bin/env python3
"""Build deterministic, architecture-specific DSM packages without Docker."""

import argparse
import gzip
import hashlib
import io
import math
import os
from pathlib import Path, PurePosixPath
import re
import struct
import subprocess
import tarfile
import tempfile
import zlib

ROOT = Path(__file__).resolve().parents[1]
ARCHITECTURES = {"amd64": "x86_64", "arm64": "armv8"}


def archive(files, epoch):
    """Use USTAR and explicit metadata; never include local xattrs or symlinks."""
    directories = set()
    for name in files:
        for parent in PurePosixPath(name).parents:
            if str(parent) != ".":
                directories.add(str(parent))
    result = io.BytesIO()
    with tarfile.open(fileobj=result, mode="w", format=tarfile.USTAR_FORMAT) as tar:
        for name in sorted(directories | files.keys()):
            info = tarfile.TarInfo(name)
            info.uid = info.gid = 0
            info.uname = info.gname = "root"
            info.mtime = epoch
            if name in directories:
                info.type = tarfile.DIRTYPE
                info.mode = 0o755
                tar.addfile(info)
            else:
                data, info.mode = files[name]
                info.size = len(data)
                tar.addfile(info, io.BytesIO(data))
    return result.getvalue()


def icon_png(size):
    """Code-native bridge mark, independent of Plex's product logo."""
    rows = bytearray()
    for y in range(size):
        rows.append(0)
        for x in range(size):
            u, v = (x + .5) / size, (y + .5) / size
            color = (25, 37, 56, 255)
            radius = math.hypot(u - .5, v - .55)
            if (.24 <= u <= .32 or .68 <= u <= .76) and .45 <= v <= .79:
                color = (222, 234, 245, 255)
            if .19 <= radius <= .27 and v <= .55:
                color = (222, 234, 245, 255)
            if .17 <= u <= .83 and .76 <= v <= .83:
                color = (64, 190, 169, 255)
            rows.extend(color)

    def chunk(kind, data):
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))

    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(bytes(rows), 9)) + chunk(b"IEND", b""))


def package(binary, arch, version, epoch):
    payload_files = {
        "bin/plex-local-bridge": (binary, 0o755),
        "etc/config.example.json": ((ROOT / "config.example.json").read_bytes(), 0o644),
        "README.md": ((ROOT / "README.md").read_bytes(), 0o644),
        "doc/DETAILS.md": ((ROOT / "doc/DETAILS.md").read_bytes(), 0o644),
        "LICENSE": ((ROOT / "LICENSE").read_bytes(), 0o644),
        "admin/gatewayctl.sh": ((ROOT / "admin/gatewayctl.sh").read_bytes(), 0o755),
        "ui/api.cgi": (binary, 0o755),
    }
    for name in ("config", "index.html", "style.css", "app.js", "dsm-session.js"):
        data = (ROOT/"ui"/name).read_bytes()
        if name == "index.html":
            for asset in ("style.css", "app.js", "dsm-session.js"):
                digest = hashlib.sha256((ROOT/"ui"/asset).read_bytes()).hexdigest()[:12]
                data = data.replace(('"'+asset+'"').encode(), ('"'+asset+'?v='+digest+'"').encode())
        payload_files["ui/"+name] = (data, 0o644)
    desktop_script = (ROOT/"ui/dsm-bridge.js").read_bytes()+b"\n"+(ROOT/"ui/desktop.js").read_bytes()
    desktop_name = "PlexLocalBridge."+hashlib.sha256(desktop_script).hexdigest()[:12]+".js"
    payload_files["ui/"+desktop_name] = (desktop_script, 0o644)
    app_config = payload_files["ui/config"][0].replace(b'"PlexLocalBridge.js"', ('"'+desktop_name+'"').encode())
    payload_files["ui/config"] = (app_config, 0o644)
    for size in (16,24,32,48,64,72,128,256):
        payload_files[f"ui/images/icon_{size}.png"] = (icon_png(size),0o644)
    payload = gzip.compress(archive(payload_files, epoch), mtime=0)
    fields = {
        "package": "PlexLocalBridge", "displayname": "Plex Local Bridge",
        "version": version, "arch": ARCHITECTURES[arch], "os_min_ver": "7.2.2-72806",
        "maintainer": "Plex Local Bridge contributors",
        "dsmuidir": "ui", "dsmappname": "SYNO.SDS.PlexLocalBridge.Instance",
        "description": "Experimental local streaming proxy for native Plex Media Server. Independent project; not affiliated with Plex.",
        "description_chs": "面向群晖原生 Plex 的实验性本机流媒体代理。独立项目，与 Plex 官方无关联。",
        "thirdparty": "yes", "beta": "yes", "ctl_stop": "yes", "ctl_uninstall": "yes",
        "install_dep_packages": "PlexMediaServer", "start_dep_services": "network-online.target",
        "checksum": hashlib.md5(payload).hexdigest(),
        "extractsize": str((sum(len(data) for data, _ in payload_files.values()) + 1023) // 1024),
    }
    info = "".join(f'{key}="{value}"\n' for key, value in fields.items()).encode()
    files = {"INFO": (info, 0o644), "package.tgz": (payload, 0o644),
             "PACKAGE_ICON.PNG": (icon_png(64), 0o644), "PACKAGE_ICON_256.PNG": (icon_png(256), 0o644)}
    for path in sorted((ROOT / "synology").rglob("*")):
        if path.is_file():
            name = path.relative_to(ROOT / "synology").as_posix()
            files[name] = (path.read_bytes(), 0o755 if name.startswith("scripts/") else 0o644)
    return archive(files, epoch)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--arch", choices=ARCHITECTURES, default="amd64")
    parser.add_argument("--version", default=(ROOT / "VERSION").read_text().strip())
    parser.add_argument("--output", type=Path, default=ROOT / "dist")
    args = parser.parse_args()
    if not re.fullmatch(r"\d+(?:\.\d+){1,3}-\d+", args.version):
        parser.error("version must have numeric components, for example 0.1.0-0001")
    epoch = int(os.environ.get("SOURCE_DATE_EPOCH", "0"))
    if epoch < 0:
        parser.error("SOURCE_DATE_EPOCH must be non-negative")
    with tempfile.TemporaryDirectory(prefix="plex-local-bridge-build-") as directory:
        binary = Path(directory) / "plex-local-bridge"
        env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=args.arch, GOTOOLCHAIN="local", GOAMD64="v1")
        subprocess.run(["go", "build", "-trimpath", "-buildvcs=false", "-ldflags",
                        f"-s -w -buildid= -X main.version={args.version}", "-o", str(binary), "./cmd/plex-local-bridge"],
                       cwd=ROOT, env=env, check=True)
        data = package(binary.read_bytes(), args.arch, args.version, epoch)
    args.output.mkdir(parents=True, exist_ok=True)
    path = args.output / f"plex-local-bridge-{args.version}-{ARCHITECTURES[args.arch]}.spk"
    path.write_bytes(data)
    digest = hashlib.sha256(data).hexdigest()
    path.with_suffix(".spk.sha256").write_text(f"{digest}  {path.name}\n")
    print(path)
    print(f"SHA256: {digest}")


if __name__ == "__main__":
    main()
