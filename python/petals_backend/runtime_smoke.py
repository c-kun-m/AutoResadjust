"""Exercise actual private runtime processes on one GPU, not capacity/performance."""
import os
os.environ["HF_HUB_OFFLINE"] = "1"
os.environ["TRANSFORMERS_OFFLINE"] = "1"

import argparse
import http.client
import json
import signal
import socket
import subprocess
import sys
import tempfile
import time
import uuid
from pathlib import Path
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from .identity import ensure
from .fault_network import FaultNetwork
from .prepare import seal
from .smoke import stop_worker


def port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def make_model(root):
    import torch
    from tokenizers import Tokenizer
    from tokenizers.models import WordLevel
    from tokenizers.pre_tokenizers import Whitespace
    from transformers import LlamaConfig, LlamaForCausalLM, PreTrainedTokenizerFast
    torch.manual_seed(42)
    model = LlamaForCausalLM(LlamaConfig(hidden_size=256, intermediate_size=512, num_hidden_layers=2,
                                        num_attention_heads=4, num_key_value_heads=4, vocab_size=128,
                                        max_position_embeddings=256, eos_token_id=None))
    model.save_pretrained(root, safe_serialization=True)
    vocab = {"<unk>": 0, "<bos>": 1, "<eos>": 2, "Hello": 3, **{f"word{i}": i for i in range(4, 128)}}
    raw = Tokenizer(WordLevel(vocab, unk_token="<unk>"))
    raw.pre_tokenizer = Whitespace()
    tokenizer = PreTrainedTokenizerFast(tokenizer_object=raw, unk_token="<unk>", bos_token="<bos>")
    tokenizer.chat_template = "{% for m in messages %}{{m['content'] + ' '}}{% endfor %}"
    tokenizer.save_pretrained(root)
    return seal(root, revision="synthetic-runtime-v1", tokenizer_revision="synthetic-runtime-v1", quantization="none")[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=Path("/tmp/petals-runtime-validation"))
    parser.add_argument("--failure-mode", choices=("kill", "pause"), default="kill",
                        help="pause freezes the worker process group to exercise stalled RPC deadlines")
    parser.add_argument("--benchmark-client", type=Path,
                        help="optional path to scripts/benchmark_inference.py for a client protocol smoke")
    parser.add_argument("--delay-ms", type=float, default=0, help="test-only one-way worker network delay, max 500 ms")
    parser.add_argument("--rate-mbps", type=float, default=0, help="test-only network rate; 0 means no rate limit")
    parser.add_argument("--loss-percent", type=float, default=0, help="test-only packet loss, max 5 percent")
    args = parser.parse_args()
    args.output_dir.mkdir(parents=True, exist_ok=True)
    root = Path(tempfile.mkdtemp(prefix="runtime-", dir=args.output_dir))
    processes, logs = [], []
    fault_network = FaultNetwork(args.delay_ms, args.rate_mbps, args.loss_percent)

    def launch(module, extra, name):
        log = (root / (name + ".log")).open("w")
        logs.append(log)
        process = subprocess.Popen([sys.executable, "-m", module, *extra], stdout=log,
                                   stderr=subprocess.STDOUT, start_new_session=True)
        processes.append(process)
        return process

    def health(number):
        try:
            with urlopen(f"http://127.0.0.1:{number}/health", timeout=3) as response:
                return json.load(response)
        except HTTPError as error:
            return json.load(error)
        except OSError:
            return {"ready": False}

    def wait_ready(numbers):
        began = time.monotonic()
        while time.monotonic() - began < 100:
            if any(p.poll() is not None for p in processes):
                raise RuntimeError(f"runtime exited; inspect {root}")
            if all(health(p)["ready"] for p in numbers):
                return
            time.sleep(.5)
        raise RuntimeError(f"runtime readiness timeout; inspect {root}")

    try:
        checksum = make_model(root / "model")
        bootstrap_port, bootstrap_http = port(), port()
        identity = root / "bootstrap.key"
        bootstrap_peer = ensure(identity)
        address = f"/ip4/127.0.0.1/tcp/{bootstrap_port}/p2p/{bootstrap_peer}"
        launch("petals_backend.bootstrap", ["--identity", str(identity), "--advertise-ip", "127.0.0.1",
               "--port", str(bootstrap_port), "--monitor-port", str(bootstrap_http)], "bootstrap")
        wait_ready([bootstrap_http])
        common = dict(deployment_id="runtime-test", prefix="collab-" + uuid.uuid4().hex,
                      model_dir=str(root / "model"), manifest_sha256=checksum,
                      initial_peers=[address], context_size=256)
        workers, worker_ports, placements, p2p_ports = [], [], [], []
        for index in range(2):
            identity = root / f"worker{index}.key"
            peer_id = ensure(identity)
            config = {**common, "http_port": port(), "p2p_port": port(), "advertise_ip": "127.0.0.1",
                      "identity_path": str(identity), "peer_id": peer_id, "start_block": index,
                      "end_block": index + 1, "kv_cache_mib": 16}
            path = root / f"worker{index}.json"
            path.write_text(json.dumps(config))
            workers.append(launch("petals_backend.worker", ["--config", str(path)], f"worker{index}"))
            worker_ports.append(config["http_port"])
            p2p_ports.append(config["p2p_port"])
            placements.append({key: config[key] for key in ("peer_id", "start_block", "end_block")})
        wait_ready(worker_ports)
        gateway_port = port()
        path = root / "gateway.json"
        path.write_text(json.dumps({**common, "http_port": gateway_port, "placements": placements}))
        launch("petals_backend.gateway", ["--config", str(path)], "gateway")
        wait_ready([gateway_port])
        url = f"http://127.0.0.1:{gateway_port}/v1/chat/completions"
        body = dict(model="runtime-test", messages=[dict(role="user", content="Hello")], max_tokens=8, temperature=0)
        with urlopen(Request(url, data=json.dumps(body).encode(), headers={"Content-Type": "application/json"}), timeout=20) as response:
            result = json.load(response)
            assert result["usage"] == {"prompt_tokens": 1, "completion_tokens": 8, "total_tokens": 9}, result
        with urlopen(Request(url, data=json.dumps({**body, "stream": True}).encode()), timeout=20) as response:
            stream = response.read().decode()
            assert '"finish_reason": "length"' in stream and "data: [DONE]" in stream, stream
        # Close immediately after the role event while prefill/decode is owned.
        connection = http.client.HTTPConnection("127.0.0.1", gateway_port, timeout=20)
        connection.request("POST", "/v1/chat/completions", json.dumps({**body, "stream": True, "max_tokens": 128}))
        response = connection.getresponse()
        assert response.status == 200
        response.readline()
        connection.sock.shutdown(socket.SHUT_RDWR) if connection.sock else None
        response.close()
        connection.close()
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            stats = health(gateway_port)
            if stats["active_requests"] == 0 and stats["canceled_total"] == 1:
                break
            time.sleep(.2)
        else:
            raise AssertionError(f"cancellation failed: {stats}")
        before_failure = [health(number) for number in worker_ports]
        assert all(s["rx_bytes_total"] > 0 and s["tx_bytes_total"] > 0 and s["steps_total"] > 0 for s in before_failure), before_failure
        benchmark_result = None
        fault_network.start(p2p_ports)
        if args.benchmark_client:
            config = {"endpoints": [{"name": "synthetic-petals", "url": url, "model": "runtime-test", "backend": "petals",
                       "model_revision": "synthetic-runtime-v1", "tokenizer_revision": "synthetic-runtime-v1",
                       "engine_version": "private-runtime-smoke", "artifact_sha256": checksum, "precision": "fp16"}],
                      "cases": [{"id": "hello", "messages": body["messages"]}]}
            config_path, report_path = root / "benchmark-config.json", root / "benchmark-result.json"
            config_path.write_text(json.dumps(config))
            with (root / "benchmark-client.log").open("w") as benchmark_log:
                subprocess.run([sys.executable, str(args.benchmark_client), str(config_path), "--output", str(report_path),
                                "--requests", "3", "--warmup", "1", "--input-tokens", "1", "--max-tokens", "8"],
                               check=True, timeout=90, stdout=benchmark_log)
            benchmark_result = json.loads(report_path.read_text())["summary"]["synthetic-petals"]
            assert benchmark_result["completed"] == 3 and not benchmark_result["complete_warmed_sample"], benchmark_result
        with urlopen(Request(url, data=json.dumps({**body, "stream": True, "max_tokens": 128}).encode()), timeout=20) as response:
            while True:
                line = response.readline()
                assert line, "stream ended before content"
                if b'"content"' in line:
                    break
            began = time.monotonic()
            if args.failure_mode == "pause":
                os.killpg(workers[1].pid, signal.SIGSTOP)
            else:
                stop_worker(workers[1], crash=True)
            try:
                tail = response.read().decode()
            finally:
                if args.failure_mode == "pause":
                    os.killpg(workers[1].pid, signal.SIGCONT)
            assert '"error"' in tail and "[DONE]" not in tail, tail
            failed_in = time.monotonic() - began
            assert failed_in < 15
        summary = {"scope": "one physical GPU, two fixed worker processes; not multi-host acceptance",
                   "json_usage": result["usage"], "sse_completed": True, "client_cancel_released_session": True,
                   "worker_metrics_before_failure": before_failure, "gateway": health(gateway_port),
                   "failure_mode": args.failure_mode, "benchmark_client_protocol_smoke": benchmark_result,
                   "network_shaping": fault_network.values,
                   "network_shaping_counters": fault_network.statistics(),
                   "midstream_failure_seconds": failed_in, "midstream_success_suppressed": True}
        (root / "result.json").write_text(json.dumps(summary, indent=2))
        print(json.dumps({"result_file": str(root / "result.json"), **summary}, indent=2))
    finally:
        try:
            fault_network.close()
        finally:
            for process in reversed(processes):
                stop_worker(process)
            for log in logs:
                log.close()


if __name__ == "__main__": main()
