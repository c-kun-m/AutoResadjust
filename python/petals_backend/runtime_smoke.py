"""Exercise actual private runtime processes on one GPU, not capacity/performance."""
import os
os.environ["HF_HUB_OFFLINE"] = "1"
os.environ["TRANSFORMERS_OFFLINE"] = "1"

import argparse
import http.client
import json
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
    args = parser.parse_args()
    args.output_dir.mkdir(parents=True, exist_ok=True)
    root = Path(tempfile.mkdtemp(prefix="runtime-", dir=args.output_dir))
    processes, logs = [], []

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
        workers, worker_ports, placements = [], [], []
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
        with urlopen(Request(url, data=json.dumps({**body, "stream": True, "max_tokens": 128}).encode()), timeout=20) as response:
            while True:
                line = response.readline()
                assert line, "stream ended before content"
                if b'"content"' in line:
                    break
            began = time.monotonic()
            stop_worker(workers[1], crash=True)
            tail = response.read().decode()
            assert '"error"' in tail and "[DONE]" not in tail, tail
            failed_in = time.monotonic() - began
            assert failed_in < 15
        summary = {"scope": "one physical GPU, two fixed worker processes; not multi-host acceptance",
                   "json_usage": result["usage"], "sse_completed": True, "client_cancel_released_session": True,
                   "worker_metrics_before_failure": before_failure, "gateway": health(gateway_port),
                   "midstream_failure_seconds": failed_in, "midstream_success_suppressed": True}
        (root / "result.json").write_text(json.dumps(summary, indent=2))
        print(json.dumps({"result_file": str(root / "result.json"), **summary}, indent=2))
    finally:
        for process in reversed(processes):
            stop_worker(process)
        for log in logs:
            log.close()


if __name__ == "__main__": main()
