"""Offline compatibility and failure test; not a multi-host performance test.

Two worker processes deliberately share ONE physical GPU and tiny random
weights. Production scheduling must never duplicate this device identity.
"""
import os
os.environ["HF_HUB_OFFLINE"] = "1"
os.environ["TRANSFORMERS_OFFLINE"] = "1"

import argparse
import json
import signal
import subprocess
import sys
import tempfile
import time
from pathlib import Path

import torch
from hivemind import DHT
from transformers import LlamaConfig, LlamaForCausalLM
from petals.data_structures import ServerState
from petals.models.llama.config import DistributedLlamaConfig
from petals.server.block_utils import get_model_block
from petals.server.server import Server
from petals.utils.convert_block import QuantType, convert_block
from petals.utils.dht import get_remote_module_infos

from .network import private_bootstrap, private_dht
from .private_model import PrivateLlama

PREFIX = "collab-private-smoke"


def block_matrix():
    config = DistributedLlamaConfig(hidden_size=256, intermediate_size=512,
                                   num_hidden_layers=2, num_attention_heads=4,
                                   num_key_value_heads=4, vocab_size=128,
                                   max_position_embeddings=128, initial_peers=[])
    results = []
    for quant in (QuantType.NONE, QuantType.INT8, QuantType.NF4):
        torch.manual_seed(42)
        block = get_model_block(config).to(dtype=torch.float16).eval()
        block = convert_block(block, 0, config, [torch.device("cuda:0")],
                              torch.device("cuda:0"), quant_type=quant, freeze=True)
        cache = None
        with torch.inference_mode():
            for length in (8, 1, 1, 1):
                inputs = torch.randn(1, length, 256, device="cuda", dtype=torch.float16)
                output, cache = block(inputs, layer_past=cache, use_cache=True)
                torch.cuda.synchronize()
                assert output.shape == inputs.shape and torch.isfinite(output).all()
                assert all(torch.isfinite(value).all() for value in cache)
        results.append({"quantization": quant.name, "prefill": 8, "decode_steps": 3,
                        "finite": True, "kv_shapes": [list(value.shape) for value in cache]})
        del block, cache, output
        torch.cuda.empty_cache()
    return results


def worker(index, bootstrap, root):
    private_bootstrap(bootstrap)
    server = Server(
        initial_peers=[bootstrap], dht_prefix=PREFIX,
        converted_model_name_or_path=str(root / "model"), throughput=1.0,
        # The test uses a constant route score, NOT a measured throughput.
        block_indices=f"{index}:{index+1}", num_handlers=1,
        device="cuda:0", torch_dtype="float16", quant_type=QuantType.NONE,
        attn_cache_tokens=128, inference_max_length=64, max_batch_size=64,
        max_alloc_timeout=5, session_timeout=15, step_timeout=5,
        request_timeout=5, update_period=2, balance_quality=0.0,
        skip_reachability_check=True, reachable_via_relay=False,
        use_relay=False, use_auto_relay=False,
        host_maddrs=["/ip4/127.0.0.1/tcp/0"],
    )
    (root / f"worker{index}.json").write_text(json.dumps({"peer_id": str(server.dht.peer_id)}))
    signal.signal(signal.SIGTERM, lambda *_: server.stop.set())
    signal.signal(signal.SIGINT, lambda *_: server.stop.set())
    try:
        server.run()
    finally:
        server.shutdown()


def stop_worker(process, crash=False):
    if process.poll() is not None:
        return
    if crash:
        os.killpg(process.pid, signal.SIGKILL)
    else:
        process.terminate()
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGKILL)
        process.wait(timeout=5)


def private_inference(root, fault_test):
    torch.manual_seed(42)
    reference = LlamaForCausalLM(LlamaConfig(
        hidden_size=256, intermediate_size=512, num_hidden_layers=2,
        num_attention_heads=4, num_key_value_heads=4, vocab_size=128,
        max_position_embeddings=128,
    )).eval()
    reference.save_pretrained(root / "model", safe_serialization=True)
    bootstrap = DHT(initial_peers=[], host_maddrs=["/ip4/127.0.0.1/tcp/0"],
                    use_relay=False, use_auto_relay=False, start=True)
    address = private_bootstrap(str(bootstrap.get_visible_maddrs()[0]))
    processes, logs = [], []
    client_dht = model = None
    try:
        for index in range(2):
            log = (root / f"worker{index}.log").open("w")
            logs.append(log)
            processes.append(subprocess.Popen(
                [sys.executable, "-m", "petals_backend.smoke", "--worker", str(index),
                 "--bootstrap", address, "--work-dir", str(root)],
                stdout=log, stderr=subprocess.STDOUT, start_new_session=True,
            ))
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            if any(process.poll() is not None for process in processes):
                raise RuntimeError(f"worker exited; inspect {root}")
            infos = get_remote_module_infos(bootstrap, [PREFIX+".0", PREFIX+".1"], latest=True)
            if all(info and any(s.state == ServerState.ONLINE for s in info.servers.values()) for info in infos):
                break
            time.sleep(1)
        else:
            raise RuntimeError("online block coverage timed out")
        allowed = [json.loads((root / f"worker{i}.json").read_text())["peer_id"] for i in range(2)]
        client_dht = private_dht([address], host_maddrs=["/ip4/127.0.0.1/tcp/0"],
                                  client_mode=True, start=True)
        config = DistributedLlamaConfig.from_pretrained(
            str(root / "model"), initial_peers=[address], dht_prefix=PREFIX,
            allowed_servers=allowed, max_retries=1, connect_timeout=3,
            request_timeout=5, update_period=2,
        )
        model = PrivateLlama.from_pretrained(str(root / "model"), config=config,
                                            torch_dtype=torch.float32, dht=client_dht)
        inputs = torch.tensor([[4, 5, 6, 7]])
        with torch.inference_mode():
            actual = model(inputs).logits
            expected = reference(inputs).logits
            max_error = (actual - expected).abs().max().item()
            assert torch.allclose(actual, expected, atol=.02, rtol=.02), max_error
            with model.inference_session(max_length=8):
                generated = model.generate(inputs, max_new_tokens=3, do_sample=False, pad_token_id=0)
        report = {"fixed_blocks": ["0:1", "1:2"], "max_logit_error": max_error,
                  "generated_ids": generated.tolist(), "allowlisted_workers": len(allowed)}
        if fault_test:
            with torch.inference_mode(), model.transformer.h.inference_session(max_length=8) as session:
                session.step(model.model.embed_tokens(inputs))
                stop_worker(processes[1], crash=True)
                began = time.monotonic()
                try:
                    session.step(model.model.embed_tokens(torch.tensor([[8]])))
                except Exception as error:
                    elapsed = time.monotonic() - began
                    assert elapsed < 20, elapsed
                    report["worker_crash"] = {"failed_within_seconds": elapsed, "error_type": type(error).__name__}
                else:
                    raise AssertionError("missing block silently accepted")
        return report
    finally:
        if model is not None:
            model.close()
        if client_dht is not None:
            client_dht.shutdown()
        for process in processes:
            stop_worker(process)
        for log in logs:
            log.close()
        bootstrap.shutdown()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--worker", type=int, choices=(0, 1))
    parser.add_argument("--bootstrap")
    parser.add_argument("--work-dir", type=Path)
    parser.add_argument("--output-dir", type=Path, default=Path("/tmp/petals-validation"))
    parser.add_argument("--fault-test", action="store_true")
    args = parser.parse_args()
    if args.worker is not None:
        if not args.bootstrap or args.work_dir is None:
            parser.error("worker requires bootstrap and work-dir")
        worker(args.worker, args.bootstrap, args.work_dir)
        return
    args.output_dir.mkdir(parents=True, exist_ok=True)
    root = Path(tempfile.mkdtemp(prefix="run-", dir=args.output_dir))
    report = {"scope": "two processes on ONE physical GPU, random tiny model; not a performance acceptance",
              "gpu": torch.cuda.get_device_name(0), "torch": torch.__version__,
              "blocks": block_matrix()}
    report["private_inference"] = private_inference(root, args.fault_test)
    (root / "result.json").write_text(json.dumps(report, indent=2))
    print(json.dumps({"result_file": str(root / "result.json"), **report}, indent=2), flush=True)


if __name__ == "__main__":
    main()
