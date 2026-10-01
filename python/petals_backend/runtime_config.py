"""Assignment validation shared by the worker and gateway executables."""
import ipaddress
import re
from pathlib import Path

from .network import _ALLOWED, _PEER, private_bootstrap

PROFILE = "petals-22afba6-torch2.8-cu128-bnb0.48.1-v1"


def private_ip(value):
    ip = ipaddress.ip_address(value)
    if not any(ip in subnet for subnet in _ALLOWED):
        raise ValueError("advertise address must be a literal private or loopback IP")
    return str(ip)


def multiaddr(host, port):
    ip = ipaddress.ip_address(host)
    return f"/ip{ip.version}/{ip}/tcp/{port}"


def integer(value, low, high, name):
    if type(value) is not int or not low <= value <= high:
        raise ValueError(f"{name} must be an integer in {low}..{high}")
    return value


def validate(config, role):
    if not isinstance(config, dict) or role not in ("worker", "gateway"):
        raise ValueError("invalid runtime configuration")
    if not re.fullmatch(r"[a-zA-Z0-9_-]{1,120}", config.get("deployment_id", "")):
        raise ValueError("invalid deployment identity")
    if not re.fullmatch(r"collab-[0-9a-f]{32,64}", config.get("prefix", "")):
        raise ValueError("prefix must identify the immutable assignment, without its lease token")
    if not re.fullmatch(r"[0-9a-f]{64}", config.get("manifest_sha256", "")):
        raise ValueError("manifest digest required")
    if not Path(config.get("model_dir", "")).is_absolute():
        raise ValueError("absolute model directory required")
    peers = config.get("initial_peers")
    if not isinstance(peers, list) or not 1 <= len(peers) <= 8:
        raise ValueError("1..8 explicit bootstrap peers required")
    for peer in peers:
        private_bootstrap(peer)
    integer(config.get("context_size"), 128, 131072, "context_size")
    integer(config.get("http_port"), 1, 65535, "http_port")
    if role == "worker":
        integer(config.get("start_block"), 0, 1023, "start_block")
        integer(config.get("end_block"), config["start_block"] + 1, 1024, "end_block")
        integer(config.get("kv_cache_mib"), 1, 1 << 20, "kv_cache_mib")
        integer(config.get("p2p_port"), 1, 65535, "p2p_port")
        private_ip(config.get("advertise_ip"))
        if not Path(config.get("identity_path", "")).is_absolute() or not _PEER.fullmatch(config.get("peer_id", "")):
            raise ValueError("persistent private identity and expected peer ID required")
    else:
        assignments = config.get("placements")
        if not isinstance(assignments, list) or not 1 <= len(assignments) <= 8:
            raise ValueError("fixed block placements required")
        end, seen = 0, set()
        for placement in assignments:
            peer = placement.get("peer_id", "")
            if not _PEER.fullmatch(peer) or peer in seen or placement.get("start_block") != end:
                raise ValueError("placements must use unique peers and consecutive blocks")
            seen.add(peer)
            end = integer(placement.get("end_block"), end + 1, 1024, "end_block")
    return config
