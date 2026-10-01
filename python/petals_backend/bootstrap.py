"""Standalone private DHT; expose its public peer address, never its key."""
import argparse
import json
import signal
import threading
import uuid
from pathlib import Path

from .identity import ensure
from .monitor import Monitor, container_traffic
from .runtime_config import multiaddr, private_ip, integer, PROFILE


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--identity", type=Path, required=True)
    parser.add_argument("--advertise-ip", required=True)
    parser.add_argument("--port", type=int, default=31330)
    parser.add_argument("--monitor-port", type=int, default=31331)
    parser.add_argument("--monitor-host", default="127.0.0.1", choices=("127.0.0.1", "0.0.0.0"))
    parser.add_argument("--address-file", type=Path)
    args = parser.parse_args()
    host = private_ip(args.advertise_ip)
    integer(args.port, 1, 65535, "port")
    integer(args.monitor_port, 1, 65535, "monitor_port")
    peer = ensure(args.identity)
    from hivemind import DHT
    dht = DHT(initial_peers=[], identity_path=str(args.identity),
              host_maddrs=[multiaddr("0.0.0.0" if ":" not in host else "::", args.port)],
              announce_maddrs=[multiaddr(host, args.port)],
              use_relay=False, use_auto_relay=False, start=True)
    stopping = threading.Event()
    data = {"profile": PROFILE, "peer_id": peer, "address": multiaddr(host, args.port) + "/p2p/" + peer, "instance": uuid.uuid4().hex}
    monitor = Monitor(args.monitor_port, lambda: {**data, **container_traffic(), "ready": dht.is_alive() and not stopping.is_set()}, host=args.monitor_host)
    signal.signal(signal.SIGTERM, lambda *_: stopping.set())
    signal.signal(signal.SIGINT, lambda *_: stopping.set())
    try:
        if args.address_file:
            args.address_file.write_text(json.dumps(data))
        print(json.dumps(data), flush=True)
        while not stopping.wait(1):
            if not dht.is_alive():
                raise RuntimeError("private DHT process exited")
    finally:
        dht.shutdown()
        monitor.close()


if __name__ == "__main__":
    main()
