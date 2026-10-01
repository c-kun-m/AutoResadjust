"""Fixed-block, offline Petals worker controlled by an Agent assignment."""
import os
os.environ["HF_HUB_OFFLINE"] = "1"
os.environ["TRANSFORMERS_OFFLINE"] = "1"

import argparse
import functools
import json
import signal
import threading
from pathlib import Path

from .artifacts import verify, verify_assignment_metadata
from .monitor import Counters, Monitor
from .runtime_config import PROFILE, multiaddr, validate


def run(config):
    config = validate(config, "worker")
    counters = Counters()
    server = None
    state = {"ready": False, "state": "verifying", "profile": PROFILE,
             "peer_id": config["peer_id"], "start_block": config["start_block"],
             "end_block": config["end_block"]}

    def snapshot():
        data = {**state, **counters.snapshot()}
        container = server.module_container if server is not None else None
        if container is not None:
            data["ready"] = container.ready.is_set() and container.is_healthy() and not server.stop.is_set()
            data["state"] = "ready" if data["ready"] else "loading"
            cache = next(iter(container.module_backends.values())).memory_cache
            data.update(kv_cache_bytes=cache.current_size_bytes, kv_capacity_bytes=cache.max_size_bytes,
                        cuda_allocated_bytes=torch.cuda.memory_allocated(0),
                        cuda_reserved_bytes=torch.cuda.memory_reserved(0))
        return data

    monitor = Monitor(config["http_port"], snapshot)
    try:
        manifest = verify(Path(config["model_dir"]), config["manifest_sha256"])
        verify_assignment_metadata(manifest, config)
        if config["end_block"] > manifest["layers"] or config["context_size"] > manifest["context_limit"]:
            raise ValueError("assigned blocks or context exceed the sealed model")
        import torch
        from hivemind import PeerID
        from petals.models.llama.config import DistributedLlamaConfig
        import petals.server.server as server_module
        from petals.server.throughput import measure_compute_rps
        from petals.utils.convert_block import QuantType
        from .metered_handler import MeteredHandler

        identity = Path(config["identity_path"])
        if identity.is_symlink() or not identity.is_file() or identity.stat().st_size > 65536:
            raise ValueError("persistent worker identity is missing or invalid")
        if str(PeerID.from_identity(identity.read_bytes())) != config["peer_id"]:
            raise ValueError("worker identity differs from the assigned peer ID")
        model_config = DistributedLlamaConfig.from_pretrained(config["model_dir"], local_files_only=True)
        blocks = config["end_block"] - config["start_block"]
        kv_bytes = 4 * model_config.hidden_size // model_config.num_key_value_groups * config["context_size"] * blocks
        if kv_bytes > config["kv_cache_mib"] * (1 << 20):
            raise ValueError("reserved KV memory cannot fit the assigned context")
        quant = QuantType[manifest["quantization"].upper()]
        state["state"] = "benchmarking"
        # Measure only local compute. Never run Petals' public speedtest or use
        # its unmeasured bandwidth fallback. Network admission belongs to CP.
        rate = measure_compute_rps(model_config, torch.device("cuda:0"), torch.float16,
                                   quant_type=quant, tensor_parallel_devices=[torch.device("cuda:0")],
                                   n_tokens=1, n_steps=16, inference=True)
        torch.cuda.empty_cache()
        state["compute_tokens_per_second_per_block"] = rate
        server_module.TransformerConnectionHandler = functools.partial(MeteredHandler, counters=counters)
        host = config["advertise_ip"]
        listen = "0.0.0.0" if ":" not in host else "::"
        state["state"] = "loading"
        server = server_module.Server(
            initial_peers=config["initial_peers"], dht_prefix=config["prefix"],
            converted_model_name_or_path=config["model_dir"], throughput=float(rate / blocks),
            block_indices=f"{config['start_block']}:{config['end_block']}", num_handlers=1,
            device="cuda:0", torch_dtype="float16", quant_type=quant,
            attn_cache_tokens=config["context_size"], inference_max_length=config["context_size"],
            max_batch_size=config["context_size"], max_alloc_timeout=3,
            session_timeout=125, step_timeout=10, request_timeout=5,
            update_period=2, balance_quality=0.0, mean_balance_check_period=2,
            skip_reachability_check=True, reachable_via_relay=False,
            use_relay=False, use_auto_relay=False,
            host_maddrs=[multiaddr(listen, config["p2p_port"])],
            announce_maddrs=[multiaddr(host, config["p2p_port"])], identity_path=str(identity),
        )
        if str(server.dht.peer_id) != config["peer_id"]:
            raise ValueError("DHT identity mismatch")
        signal.signal(signal.SIGTERM, lambda *_: server.stop.set())
        signal.signal(signal.SIGINT, lambda *_: server.stop.set())
        server.run()
    finally:
        state.update(ready=False, state="stopped")
        if server is not None:
            server.stop.set()
            server.shutdown()
        monitor.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, required=True)
    args = parser.parse_args()
    if args.config.stat().st_size > 65536:
        raise ValueError("configuration too large")
    run(json.loads(args.config.read_text()))


if __name__ == "__main__":
    main()
