"""Apply the one tested compatibility change, preserving dependency validation."""
import hashlib
from pathlib import Path


def replace_once(path, old, new):
    text = path.read_text()
    if text.count(old) != 1:
        raise RuntimeError(f"unexpected upstream source in {path}")
    path.write_text(text.replace(old, new))


root = Path("/sources")
replace_once(root / "petals/setup.cfg", "bitsandbytes==0.41.1", "bitsandbytes==0.48.1")
replace_once(root / "petals/setup.cfg",
             "hivemind @ git+https://github.com/learning-at-home/hivemind.git@213bff98a62accb91f254e2afdccbf1d69ebdea9",
             "hivemind==1.2.0.dev0")
replace_once(root / "hivemind/requirements.txt",
             "multiaddr @ git+https://github.com/multiformats/py-multiaddr.git@e01dbd38f2c0464c0f78b556691d655265018cce",
             "multiaddr==0.0.9")
binary = root / "hivemind/hivemind/hivemind_cli/p2pd"
assert hashlib.sha256(binary.read_bytes()).hexdigest() == "42f8f48e62583b97cdba3c31439c08029fb2b9fc506b5bdd82c46b7cc1d279d8"
binary.chmod(0o755)
