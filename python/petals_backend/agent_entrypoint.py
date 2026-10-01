"""Prepare a stable peer identity, then let the Go Agent own all runtimes."""
import os
import sys
from pathlib import Path

from .identity import ensure
from .runtime_config import PROFILE


def main():
    path = Path(os.environ.get("PETALS_IDENTITY", "/state/petals/node.key"))
    peer = ensure(path)
    initial = os.environ.get("PETALS_INITIAL_PEERS", "")
    if not initial:
        raise ValueError("PETALS_INITIAL_PEERS must name your private bootstrap")
    args = ["/usr/local/bin/edge-agent", "--petals-python", sys.executable,
            "--petals-peer-id", peer, "--petals-identity", str(path),
            "--petals-initial-peers", initial, "--petals-state-dir", str(path.parent / "assignments"),
            "--engine-version", PROFILE, *sys.argv[1:]]
    os.execv(args[0], args)


if __name__ == "__main__": main()
