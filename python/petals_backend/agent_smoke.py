"""Real Agent/Petals lease test; fake control transport, ONE physical GPU.

The second block is an independent helper, not a second registered resource.
This intentionally cannot replace platform multi-host scheduling acceptance.
"""
import os
os.environ["HF_HUB_OFFLINE"] = "1"
os.environ["TRANSFORMERS_OFFLINE"] = "1"

import argparse
import hashlib
import json
import subprocess
import sys
import tempfile
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from .identity import ensure
from .runtime_config import PROFILE
from .runtime_smoke import make_model, port
from .smoke import stop_worker


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=Path("/tmp/petals-agent-validation"))
    args = parser.parse_args()
    args.output_dir.mkdir(parents=True, exist_ok=True)
    root = Path(tempfile.mkdtemp(prefix="agent-", dir=args.output_dir))
    processes, logs = [], []
    lock = threading.Lock()
    control = {"unavailable": False, "stop": False, "reports": [], "services": [], "observed_states": []}
    auth = uuid.uuid4().hex
    assignment = None

    def launch(module, extra, name, env=None):
        log = (root / (name + ".log")).open("w")
        logs.append(log)
        process = subprocess.Popen([sys.executable, "-m", module, *extra], stdout=log,
                                   stderr=subprocess.STDOUT, start_new_session=True, env=env)
        processes.append(process)
        return process

    def health(number):
        try:
            with urlopen(f"http://127.0.0.1:{number}/health", timeout=2) as response:
                return json.load(response)
        except (OSError, ValueError):
            return {"ready": False}

    def wait(check, timeout=90):
        began = time.monotonic()
        while time.monotonic() - began < timeout:
            if any(p.poll() is not None for p in processes):
                raise RuntimeError(f"runtime exited; inspect {root}")
            if check():
                return
            time.sleep(.5)
        raise AssertionError(f"condition timed out; inspect {root}")

    class ControlHandler(BaseHTTPRequestHandler):
        def do_POST(self):
            if self.headers.get("Authorization") != "Bearer " + auth:
                self.send_error(401)
                return
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            with lock:
                if control["unavailable"]:
                    self.send_error(503)
                    return
                control["reports"] = body.get("reports", [])
                control["services"] = body.get("services", [])
                control["node"] = body.get("node", {})
                for report in control["reports"]:
                    control["observed_states"].append([report["rpc_state"], report["model_state"]])
                reply = dict(assignment)
                reply["start_model"] = any(r["rpc_state"] == "ready" for r in control["reports"])
                reply["stop"] = control["stop"]
            data = json.dumps({"assignments": [reply], "network_probes": []}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *_):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), ControlHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        checksum = make_model(root / "model")
        manifest = json.loads((root / "model/platform-manifest.json").read_text())
        bootstrap_port, bootstrap_http = port(), port()
        identity = root / "bootstrap.key"
        address = f"/ip4/127.0.0.1/tcp/{bootstrap_port}/p2p/{ensure(identity)}"
        launch("petals_backend.bootstrap", ["--identity", str(identity), "--advertise-ip", "127.0.0.1",
               "--port", str(bootstrap_port), "--monitor-port", str(bootstrap_http)], "bootstrap")
        wait(lambda: health(bootstrap_http)["ready"])
        agent_http, worker_http, model_http, worker_p2p = port(), port(), port(), port()
        node_key = root / "node.key"
        peer = ensure(node_key)
        other_key = root / "other.key"
        other_peer = ensure(other_key)
        dep, lease = "dep-agent-smoke", uuid.uuid4().hex
        prefix = "collab-" + hashlib.sha256((dep + "\x00" + lease).encode()).hexdigest()
        worker_config = dict(deployment_id=dep, prefix=prefix, model_dir=str(root / "model"),
                             manifest_sha256=checksum, initial_peers=[address], context_size=128,
                             http_port=port(), p2p_port=port(), advertise_ip="127.0.0.1", identity_path=str(other_key),
                             peer_id=other_peer, start_block=1, end_block=2, kv_cache_mib=16)
        path = root / "other.json"
        path.write_text(json.dumps(worker_config))
        launch("petals_backend.worker", ["--config", str(path)], "other")
        wait(lambda: health(worker_config["http_port"])["ready"])
        gpu = subprocess.check_output(["nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader"], text=True).splitlines()[0].strip()
        placement = dict(node_id="agent-smoke", gpu_id=gpu, agent_url=f"http://127.0.0.1:{agent_http}", coordinator=True,
                         start_block=0, end_block=1, petals=dict(peer_id=peer, address=f"127.0.0.1:{worker_p2p}", initial_peers=[address]))
        other = dict(node_id="external-test-helper", start_block=1, end_block=2,
                     petals=dict(peer_id=other_peer, address=f"127.0.0.1:{worker_config['p2p_port']}", initial_peers=[address]))
        spec = dict(name="lease smoke", backend="petals", model_ref="sealed-test", model_file="model", model_sha256=checksum,
                    context_size=128, kv_cache_mib=16, **{k: manifest[k] for k in ("layers", "block_mib", "load_ram_mib", "kv_bytes_per_token_per_layer")})
        assignment = dict(deployment_id=dep, token=lease, spec=spec, placement=placement, peers=[placement, other])
        env = {**os.environ, "PETALS_IDENTITY": str(node_key), "PETALS_INITIAL_PEERS": address, "CONTROL_PLANE_TOKEN": auth}
        launch("petals_backend.agent_entrypoint", ["--node-id", "agent-smoke", "--control-plane", f"http://127.0.0.1:{server.server_port}",
               "--listen", f"127.0.0.1:{agent_http}", "--advertise-host", "127.0.0.1", "--network-group", "private-smoke",
               "--petals-port", str(worker_p2p), "--petals-http", f"127.0.0.1:{worker_http}",
               "--model-backend", f"127.0.0.1:{model_http}", "--model-dir", str(root), "--interval", "1s"], "agent", env)

        def states():
            with lock:
                return [dict(r) for r in control["reports"]]

        wait(lambda: any(r["rpc_state"] == "ready" and r["model_state"] == "ready" for r in states()))
        with lock:
            assert control["node"]["agent"]["backends"] == ["petals"]
            assert control["node"]["agent"]["engine_version"] == PROFILE
        request = Request(f"http://127.0.0.1:{agent_http}/inference/v1/chat/completions",
                          data=json.dumps(dict(messages=[dict(role="user", content="Hello")], max_tokens=5, temperature=0)).encode(),
                          headers={"Content-Type": "application/json", "Authorization": "Bearer " + auth,
                                   "X-Assignment-Token": lease, "X-Deployment-ID": dep})
        with urlopen(request, timeout=20) as response:
            result = json.load(response)
            assert result["usage"]["completion_tokens"] == 5
        wait(lambda: any(s.get("kind") == "petals-worker" and s.get("rx_bytes", 0) > 0 for s in control["services"]), timeout=10)
        with lock:
            captured = json.loads(json.dumps(control["services"]))
            control["unavailable"] = True
        began = time.monotonic()
        # The normal production lease is 45 seconds; keep that policy in test.
        wait(lambda: not health(worker_http)["ready"] and not health(model_http)["ready"], timeout=60)
        expired_in = time.monotonic() - began
        assert 40 <= expired_in <= 60, expired_in
        try:
            urlopen(request, timeout=3)
        except HTTPError as error:
            assert error.code == 503
        else:
            raise AssertionError("expired lease still admitted inference")
        with lock:
            control["unavailable"] = False
            control["stop"] = True
        wait(lambda: any(r["rpc_state"] == "stopped" and r["model_state"] == "stopped" for r in states()), timeout=10)
        with lock:
            observed = control["observed_states"]
            assert ["failed", "failed"] in observed and ["stopped", "stopped"] in observed
        summary = {"scope": "real Agent and GPU runtimes; fake sync transport and two processes on ONE GPU",
                   "usage": result["usage"], "lease_expired_seconds": expired_in,
                   "expired_inference_status": 503, "failure_and_stop_acknowledged": True,
                   "services_before_expiry": captured}
        (root / "result.json").write_text(json.dumps(summary, indent=2))
        print(json.dumps({"result_file": str(root / "result.json"), "scope": summary["scope"],
                          "lease_expired_seconds": expired_in, "failure_and_stop_acknowledged": True}))
    finally:
        for process in reversed(processes):
            stop_worker(process)
        for log in logs:
            log.close()
        server.shutdown()
        server.server_close()


if __name__ == "__main__": main()
