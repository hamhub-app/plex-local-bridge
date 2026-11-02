import gzip
import hashlib
import importlib.util
import io
import json
from pathlib import Path, PurePosixPath
import struct
import tarfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("build_spk", ROOT / "scripts/build_spk.py")
build = importlib.util.module_from_spec(spec)
spec.loader.exec_module(build)


class PackageTests(unittest.TestCase):
    def test_package_manifest_and_permissions(self):
        for arch, synology_arch in build.ARCHITECTURES.items():
            with self.subTest(arch=arch):
                data = build.package(b"test-binary", arch, "0.1.0-0001", 0)
                with tarfile.open(fileobj=io.BytesIO(data)) as spk:
                    names = spk.getnames()
                    for name in names:
                        self.assertFalse(PurePosixPath(name).is_absolute())
                        self.assertNotIn("..", PurePosixPath(name).parts)
                        member = spk.getmember(name)
                        self.assertFalse(member.issym() or member.islnk())
                        self.assertEqual((member.uid, member.gid, member.mtime), (0, 0, 0))
                        self.assertFalse(member.pax_headers)
                    payload = spk.extractfile("package.tgz").read()
                    info = spk.extractfile("INFO").read().decode()
                    self.assertIn(f'arch="{synology_arch}"', info)
                    self.assertIn(f'checksum="{hashlib.md5(payload).hexdigest()}"', info)
                    self.assertIn('install_dep_packages="PlexMediaServer"', info)
                    self.assertIn('dsmuidir="ui"', info)
                    self.assertIn('dsmappname="SYNO.SDS.PlexLocalBridge.Instance"', info)
                    self.assertEqual(json.loads(spk.extractfile("conf/privilege").read()),
                                     {"defaults": {"run-as": "package"}})
                    for script in ("preinst", "postinst", "preuninst", "postuninst", "preupgrade", "postupgrade", "start-stop-status", "common"):
                        path = "scripts/" + script
                        self.assertEqual(spk.getmember(path).mode, 0o755)
                        self.assertTrue(spk.extractfile(path).read().startswith(b"#!/bin/sh\n"))
                    for name, size in (("PACKAGE_ICON.PNG", 64), ("PACKAGE_ICON_256.PNG", 256)):
                        icon = spk.extractfile(name).read()
                        self.assertEqual(icon[:8], b"\x89PNG\r\n\x1a\n")
                        self.assertEqual(struct.unpack(">II", icon[16:24]), (size, size))
                with tarfile.open(fileobj=io.BytesIO(gzip.decompress(payload))) as tar:
                    self.assertEqual(tar.getmember("bin/plex-local-bridge").mode, 0o755)
                    self.assertEqual(tar.extractfile("bin/plex-local-bridge").read(), b"test-binary")
                    script = (ROOT/"ui/dsm-bridge.js").read_bytes()+b"\n"+(ROOT/"ui/desktop.js").read_bytes()
                    desktop_name = "PlexLocalBridge."+hashlib.sha256(script).hexdigest()[:12]+".js"
                    files = {"bin/plex-local-bridge", "etc/config.example.json", "README.md", "doc/DETAILS.md", "LICENSE", "admin/gatewayctl.sh",
                             "ui/api.cgi", "ui/"+desktop_name, "ui/index.html", "ui/style.css", "ui/app.js", "ui/dsm-session.js", "ui/config"}
                    files.update(f"ui/images/icon_{size}.png" for size in (16, 24, 32, 48, 64, 72, 128, 256))
                    self.assertEqual({m.name for m in tar if m.isfile()}, files)
                    self.assertEqual(tar.getmember("ui/api.cgi").mode, 0o755)
                    self.assertEqual(tar.extractfile("ui/api.cgi").read(), b"test-binary")
                    html = tar.extractfile("ui/index.html").read().decode()
                    for asset in ("style.css", "app.js", "dsm-session.js"):
                        digest = hashlib.sha256((ROOT / "ui" / asset).read_bytes()).hexdigest()[:12]
                        self.assertIn(f'"{asset}?v={digest}"', html)
                    desktop = json.loads(tar.extractfile("ui/config").read())
                    self.assertEqual(tar.extractfile("ui/"+desktop_name).read(), script)
                    app = desktop[desktop_name]["SYNO.SDS.PlexLocalBridge.Instance"]
                    self.assertFalse(app["allUsers"])
                    self.assertEqual(app["type"], "app")

    def test_unsigned_dsm_package_never_requests_root(self):
        privilege = json.loads((ROOT / "synology/conf/privilege").read_text())
        self.assertEqual(privilege, {"defaults": {"run-as": "package"}})
        for path in (ROOT / "synology/scripts").iterdir():
            text = path.read_text()
            for forbidden in ("require_root", "chown", "sudo ", "iptables", "gatewayctl", " supervise ", " recover "):
                self.assertNotIn(forbidden, text, path.name)

    def test_archive_is_reproducible(self):
        self.assertEqual(build.package(b"same", "amd64", "0.1.0-0001", 0),
                         build.package(b"same", "amd64", "0.1.0-0001", 0))

    def test_no_mutations_in_pre_hooks(self):
        for name in ("preinst", "preuninst", "preupgrade"):
            text = (ROOT / "synology/scripts" / name).read_text()
            for forbidden in ("iptables", "chown", "chmod", "mkdir", "rm ", "mv ", "init-config"):
                self.assertNotIn(forbidden, text)


if __name__ == "__main__":
    unittest.main()
