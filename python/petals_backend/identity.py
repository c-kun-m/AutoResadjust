"""Create/read a persistent libp2p identity without joining any network."""
import argparse
import json
import os
from pathlib import Path


def ensure(path):
    from hivemind import PeerID
    from hivemind.p2p import P2P
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.is_symlink():
        raise ValueError("identity must not be a symlink")
    if not path.exists():
        # Exclusive placeholder also prevents two initializers overwriting keys.
        descriptor = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        os.close(descriptor)
        P2P.generate_identity(str(path))
    if not path.is_file() or not 0 < path.stat().st_size <= 65536:
        raise ValueError("invalid identity file")
    return str(PeerID.from_identity(path.read_bytes()))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--path", type=Path, required=True)
    args = parser.parse_args()
    print(json.dumps({"peer_id": ensure(args.path)}))


if __name__ == "__main__":
    main()
