#!/usr/bin/env python3
"""Exercise real Linux NAT and package scripts in disposable Docker containers.

Only the fixture container gets NET_ADMIN. No host networking or host ports.
"""

import argparse
import io
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
BASE = "/var/packages/PlexLocalBridge"
BIN = BASE + "/target/bin/plex-local-bridge"
CONTROL = BASE + "/scripts/start-stop-status"
ADMIN = BASE + "/target/admin/gatewayctl.sh"
PRIVATE = "/usr/local/libexec/plex-local-bridge"
GATEWAY_STATE = "/var/run/plex-local-bridge"


def run(*args, check=True, input=None):
    result = subprocess.run(args, input=input, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=180)
    if check and result.returncode:
        raise RuntimeError(f"{args}: {result.stdout}\n{result.stderr}")
    return result


def main():
    architecture = run("docker", "info", "--format", "{{.Architecture}}").stdout.strip()
    default_arch = "arm64" if architecture in ("aarch64", "arm64") else "amd64"
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--arch", choices=("amd64", "arm64"), default=default_arch)
    arch = parser.parse_args().arch
    synology_arch = "armv8" if arch == "arm64" else "x86_64"
    version = (ROOT / "VERSION").read_text().strip()
    spk = ROOT / "dist" / f"plex-local-bridge-{version}-{synology_arch}.spk"
    if not spk.exists():
        raise RuntimeError(f"Build {arch} first: python3 scripts/build_spk.py --arch {arch}")
    image = "plex-local-bridge-integration:local"
    print("Building isolated Linux fixture...", flush=True)
    subprocess.run(["docker", "build", "--platform", "linux/"+arch, "--progress=plain", "-t", image,
                    str(ROOT / "tests/fixtures")], check=True, timeout=600)
    name = "plb-test-" + uuid.uuid4().hex[:10]
    server, client, network = name + "-nas", name + "-client", name + "-net"
    started = []

    def docker_exec(*args, check=True, input=None):
        return run("docker", "exec", "-i", server, *args, check=check, input=input)

    def package_exec(*args, check=True, input=None):
        return docker_exec("runuser", "-u", "PlexLocalBridge", "--", *args, check=check, input=input)

    def web_request(data=None, cookie="id=fixture", csrf="", root_launcher=False):
        body = json.dumps(data) if data is not None else ""
        env = ["GATEWAY_INTERFACE=CGI/1.1", "SERVER_PROTOCOL=HTTP/1.1", "SERVER_NAME=nas", "SERVER_PORT=5000",
               "REMOTE_ADDR=192.0.2.10", "HTTP_HOST=nas:5000", "HTTP_ORIGIN=http://nas:5000", "HTTP_COOKIE=" + cookie,
               "SCRIPT_NAME=/webman/3rdparty/PlexLocalBridge/api.cgi", "HTTP_X_PLB_CSRF=" + csrf,
               "REQUEST_METHOD=" + ("POST" if data is not None else "GET"),
               "QUERY_STRING=action=" + ("save" if data is not None else "status")]
        if data is not None:
            env += ["CONTENT_TYPE=application/json", "CONTENT_LENGTH=" + str(len(body.encode()))]
        launcher = docker_exec if root_launcher else package_exec
        output = launcher("env", *env, BASE + "/target/ui/api.cgi", input=body).stdout
        headers, response = output.split("\n\n", 1)
        status = next((int(line.split()[1]) for line in headers.splitlines() if line.startswith("Status:")), 200)
        return status, json.loads(response)

    def request(path="/media", token=True, port=32400):
        source = """
          const http = require('http');
          const req = http.get({hostname:process.argv[1],port:Number(process.argv[4]),path:process.argv[2],headers:{
            ...(process.argv[3]==='yes'?{'X-Plex-Token':'integration-token'}:{}),
            Range:'bytes=2-5','X-Forwarded-For':'203.0.113.5','X-Real-IP':'203.0.113.5'}}, r=>{
              let body='';r.on('data',c=>body+=c);r.on('end',()=>console.log(JSON.stringify({status:r.statusCode,body,headers:r.headers})));
          });req.setTimeout(4000,()=>{req.destroy();process.exit(2)});req.on('error',()=>process.exit(3));
        """
        return json.loads(run("docker", "exec", client, "node", "-e", source, server, path, "yes" if token else "no", str(port)).stdout)

    def expect_status(expected):
        result = request()
        assert result["status"] == expected, result
        if expected == 206:
            assert result["body"] == "2345", result
            assert result["headers"]["content-range"] == "bytes 2-5/36", result

    def wait_until(predicate, timeout=15):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if predicate():
                return
            time.sleep(.1)
        raise AssertionError("condition did not become true")

    try:
        run("docker", "network", "create", network)
        run("docker", "run", "-d", "--rm", "--platform", "linux/"+arch, "--name", server, "--network", network, "--cap-add", "NET_ADMIN", image)
        started.append(server)
        run("docker", "run", "-d", "--rm", "--platform", "linux/"+arch, "--name", client, "--network", network, image, "node", "-e", "setInterval(()=>{},1000)")
        started.append(client)

        def backend_ready():
            try:
                return request("/identity")["status"] == 200
            except (RuntimeError, ValueError):
                return False

        # 异架构仿真下容器已创建并不等于 Node 已完成启动。
        wait_until(backend_ready, timeout=30)
        expect_status(403)
        docker_exec("mkdir", "-p", BASE + "/target/bin", BASE + "/target/admin", BASE + "/etc", BASE + "/var")
        with tempfile.TemporaryDirectory(prefix="plb-integration-") as directory:
            binary = Path(directory) / "plex-local-bridge"
            admin = Path(directory) / "gatewayctl.sh"
            with tarfile.open(spk) as outer:
                with tarfile.open(fileobj=io.BytesIO(outer.extractfile("package.tgz").read()), mode="r:gz") as payload:
                    binary.write_bytes(payload.extractfile("bin/plex-local-bridge").read())
                    admin.write_bytes(payload.extractfile("admin/gatewayctl.sh").read())
                    for member in payload:
                        if member.isfile() and member.name.startswith("ui/"):
                            assert ".." not in Path(member.name).parts
                            target = Path(directory) / member.name
                            target.parent.mkdir(parents=True, exist_ok=True)
                            target.write_bytes(payload.extractfile(member).read())
                            target.chmod(member.mode & 0o777)
            run("docker", "cp", str(binary), server + ":" + BIN)
            run("docker", "cp", str(admin), server + ":" + ADMIN)
            run("docker", "cp", str(Path(directory)/"ui"), server + ":" + BASE + "/target/ui")
            run("docker", "cp", str(ROOT / "synology/scripts"), server + ":" + BASE + "/scripts")
            ipv6 = docker_exec("ip6tables", "-t", "nat", "-S", check=False).returncode == 0
            config = dict(upstream_url="auto", public_port=32400, listen_port=13240, intercept_ipv6=ipv6, max_connections=16)
            path = Path(directory) / "config.json"

            def rules_absent():
                families = ("iptables", "ip6tables") if ipv6 else ("iptables",)
                return all(docker_exec(binary, "-t", "nat", "-S", "PLB_GATE_V1", check=False).returncode != 0 for binary in families)

            def save_config():
                path.write_text(json.dumps(config))
                run("docker", "cp", str(path), server + ":" + BASE + "/etc/config.json")
                docker_exec("chown", "PlexLocalBridge:PlexLocalBridge", BASE+"/etc/config.json")
                package_exec("sh", BASE + "/scripts/postinst")

            # Scripts are executable in SPK; docker cp uses the source checkout modes.
            docker_exec("sh", "-c", "chmod 755 " + BASE + "/scripts/* " + BIN + " " + ADMIN)
            # 模拟 DSM defaults.run-as=package 对 FHS 和 payload 的归属处理。
            docker_exec("chown", "-R", "PlexLocalBridge:PlexLocalBridge", BASE)
            save_config()
            package_exec(CONTROL, "start")
            expect_status(403)
            assert rules_absent()
            assert request(port=13240)["status"] == 206
            assert package_exec("sh", ADMIN, "enable", check=False).returncode != 0
            assert web_request(cookie="")[0] == 401
            code, state = web_request()
            assert code == 200 and state["proxy_running"], (code, state)
            code, root_launched = web_request(root_launcher=True)
            assert code == 200 and root_launched["proxy_running"], (code, root_launched)
            assert state["approval_needed"]
            data = {"config": state["config"], "revision": state["revision"]}
            assert web_request(data=data, csrf="invalid")[0] == 403
            print("PASS: all package hooks run unprivileged; no NAT before explicit administrator activation", flush=True)
            print("PASS: CGI drops root launcher identity before authentication; DSM session and per-session CSRF required", flush=True)

            docker_exec("sh", ADMIN, "enable")
            wait_until(lambda: request()["status"] == 206)
            expect_status(206)
            assert request(token=False)["status"] == 401
            probe = json.loads(request("/probe")["body"])
            assert probe["forwarded"] == "" and probe["realIP"] == "", probe
            assert probe["host"] == probe["remote"] + ":32400", probe
            child = docker_exec("cat", BASE + "/var/run/daemon.pid").stdout.strip()
            status = docker_exec("cat", f"/proc/{child}/status").stdout
            uid_line = next(line for line in status.splitlines() if line.startswith("Uid:"))
            assert int(uid_line.split()[1]) > 0, status
            assert package_exec("test", "-w", PRIVATE+"/plex-local-bridge", check=False).returncode != 0
            assert package_exec("test", "-w", PRIVATE+"/config.json", check=False).returncode != 0
            code, state = web_request()
            assert code == 200 and state["gateway"]["phase"] == "active" and not state["approval_needed"], state
            assert package_exec("test", "-w", GATEWAY_STATE+"/status.json", check=False).returncode != 0
            print(f"PASS: administrator gateway -> Range 206; auth retained; proxy non-root; private helper protected; IPv6 NAT={ipv6}", flush=True)

            assert child.isdigit()
            docker_exec("sh", "-c", f"kill -KILL {child}")
            wait_until(rules_absent)
            expect_status(403)
            print("PASS: proxy crash removed interception and restored native Plex", flush=True)

            package_exec(CONTROL, "start")
            wait_until(lambda: request()["status"] == 206)
            expect_status(206)
            package_exec(CONTROL, "stop")
            wait_until(rules_absent)
            assert rules_absent()
            expect_status(403)
            assert package_exec(CONTROL, "status", check=False).returncode == 3
            print("PASS: restart and normal stop restored original entry", flush=True)

            package_exec(CONTROL, "start")
            code, state = web_request()
            config["listen_port"] = 13241
            data = {"config": config, "revision": state["revision"]}
            code, updated = web_request(data=data, csrf=state["csrf"])
            assert code == 200 and updated["proxy_running"] and updated["approval_needed"], (code, updated)
            assert web_request(data=data, csrf=state["csrf"])[0] == 409
            assert request(port=13241)["status"] == 206
            time.sleep(4)
            assert rules_absent()
            expect_status(403)
            package_exec(CONTROL, "stop")
            print("PASS: WebUI port save restarts proxy; rejects stale saves; gateway waits for renewed approval", flush=True)

            run("docker", "exec", "-d", server, "node", "-e", "require('http').createServer((q,s)=>s.end()).listen(32401,'0.0.0.0')")
            wait_until(lambda: docker_exec("node", "-e", "const c=require('net').connect(32401,'127.0.0.1',()=>{c.destroy();process.exit(0)});c.on('error',()=>process.exit(1))", check=False).returncode == 0)
            code, state = web_request()
            config["listen_port"] = 32401
            code, error = web_request(data={"config": config, "revision": state["revision"]}, csrf=state["csrf"])
            assert code == 409 and "端口不可用" in error["error"], (code, error)
            save_config()
            assert package_exec(CONTROL, "start", check=False).returncode != 0
            assert rules_absent()
            expect_status(403)
            print("PASS: occupied proxy port left native Plex untouched", flush=True)

            config["listen_port"] = 13240
            save_config()
            docker_exec("touch", "/tmp/allow-anonymous")
            assert package_exec(CONTROL, "start", check=False).returncode != 0
            assert rules_absent()
            docker_exec("rm", "/tmp/allow-anonymous")
            expect_status(403)
            print("PASS: anonymous-library configuration refused before interception", flush=True)

            # Recovery must work even if someone broke the configuration after stopping.
            docker_exec("sh", "-c", "echo broken > " + BASE + "/etc/config.json")
            docker_exec("sh", PRIVATE+"/gatewayctl", "stop")
            docker_exec(PRIVATE+"/plex-local-bridge", "recover", "--state", GATEWAY_STATE, "--config", BASE + "/etc/config.json")
            code, state = web_request()
            assert code == 200 and state["gateway"]["phase"] == "stopped" and state.get("config_error"), (code, state)
            print("PASS: recovery does not depend on valid configuration", flush=True)
    except Exception:
        print(run("docker", "logs", server, check=False).stderr[-3000:])
        print(docker_exec("cat", BASE + "/var/service.log", check=False).stdout[-6000:])
        print(docker_exec("cat", PRIVATE + "/gateway.log", check=False).stdout[-6000:])
        raise
    finally:
        for container in reversed(started):
            run("docker", "rm", "-f", container, check=False)
        run("docker", "network", "rm", network, check=False)


if __name__ == "__main__":
    main()
