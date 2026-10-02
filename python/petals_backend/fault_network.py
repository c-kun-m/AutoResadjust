"""Test-only network shaping, restricted to an isolated container loopback."""
import math
import json
import subprocess
from pathlib import Path


class FaultNetwork:
    def __init__(self, delay_ms=0, rate_mbps=0, loss_percent=0):
        self.values = {"one_way_delay_ms": delay_ms, "rate_mbps": rate_mbps, "loss_percent": loss_percent}
        for value, maximum in ((delay_ms, 500), (rate_mbps, 10000), (loss_percent, 5)):
            if type(value) not in (int, float) or not math.isfinite(value) or not 0 <= value <= maximum:
                raise ValueError("invalid bounded network fault settings")
        self.installed = False

    @staticmethod
    def command(*arguments):
        return subprocess.run(["tc", *arguments], check=True, timeout=5, capture_output=True, text=True)

    def start(self, ports):
        if not any(self.values.values()):
            return
        if not Path("/.dockerenv").is_file() or {p.name for p in Path("/sys/class/net").iterdir()} != {"lo"}:
            raise RuntimeError("network faults require a dedicated Docker --network none container with only lo")
        if not ports or any(type(p) is not int or not 1 <= p <= 65535 for p in ports):
            raise ValueError("fixed worker TCP ports required")
        # add, never replace: a pre-existing root policy must fail untouched.
        self.command("qdisc", "add", "dev", "lo", "root", "handle", "1:", "prio", "bands", "3", "priomap", *("0" for _ in range(16)))
        self.installed = True
        try:
            netem = ["qdisc", "add", "dev", "lo", "parent", "1:3", "handle", "30:", "netem"]
            if self.values["one_way_delay_ms"]:
                netem += ["delay", f'{self.values["one_way_delay_ms"]}ms']
            if self.values["rate_mbps"]:
                netem += ["rate", f'{self.values["rate_mbps"]}mbit']
            if self.values["loss_percent"]:
                netem += ["loss", f'{self.values["loss_percent"]}%']
            self.command(*netem)
            for number, port in enumerate(ports):
                for offset, direction in enumerate(("sport", "dport")):
                    self.command("filter", "add", "dev", "lo", "parent", "1:", "protocol", "ip", "pref", str(10 + 2 * number + offset),
                                 "u32", "match", "ip", direction, str(port), "0xffff", "flowid", "1:3")
        except BaseException:
            self.close()
            raise

    def close(self):
        if self.installed:
            self.command("qdisc", "del", "dev", "lo", "root", "handle", "1:")
            self.installed = False

    def statistics(self):
        if not self.installed:
            return None
        counters = json.loads(self.command("-s", "-j", "qdisc", "show", "dev", "lo").stdout)
        affected = next(q for q in counters if q.get("handle") == "30:")
        if affected.get("packets", 0) <= 0:
            raise AssertionError("network fault policy did not observe any worker packets")
        return affected
