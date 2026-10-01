import copy
import json
import tempfile
import unittest
from pathlib import Path
from urllib.error import HTTPError
from urllib.request import urlopen

from .gateway import chat_request
from .identity import ensure
from .monitor import Monitor
from .runtime_config import validate


class RuntimeContract(unittest.TestCase):
    def test_private_persistent_identity(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "node.key"
            first = ensure(path)
            self.assertEqual(first, ensure(path))
            self.assertNotEqual(first, ensure(Path(folder) / "another.key"))

    def test_assignment_and_coverage(self):
        peer = "Qm" + "1" * 44
        config = dict(deployment_id="dep-1", prefix="collab-" + "a" * 32,
                      manifest_sha256="b" * 64, model_dir="/models/test",
                      initial_peers=[f"/ip4/10.0.0.1/tcp/31330/p2p/{peer}"],
                      context_size=256, http_port=8080,
                      placements=[dict(peer_id=peer, start_block=0, end_block=2)])
        validate(config, "gateway")
        for patch in ({"prefix": "public"}, {"context_size": True}, {"initial_peers": []},
                      {"model_dir": "relative"}, {"http_port": 0}):
            with self.subTest(patch=patch), self.assertRaises(ValueError):
                validate({**config, **patch}, "gateway")
        broken = copy.deepcopy(config)
        broken["placements"][0]["start_block"] = 1
        with self.assertRaises(ValueError): validate(broken, "gateway")
        duplicate = copy.deepcopy(config)
        duplicate["placements"].append(dict(peer_id=peer, start_block=2, end_block=3))
        with self.assertRaises(ValueError): validate(duplicate, "gateway")

    def test_chat_limits(self):
        request = {"messages": [{"role": "user", "content": "Hello"}], "max_tokens": 4}
        self.assertEqual(chat_request(request, 256)[1], 4)
        for patch in ({"n": 2}, {"stream": 1}, {"temperature": float("nan")}, {"tools": []},
                      {"messages": []}, {"max_tokens": -1}, {"max_tokens": True},
                      {"max_completion_tokens": 4}, {"top_p": 0}, {"stream_options": []}):
            with self.subTest(patch=patch), self.assertRaises(ValueError):
                chat_request({**request, **patch}, 256)

    def test_health_and_measured_fields(self):
        state = {"ready": False, "peer_id": "private-id", "rx_bytes_total": 37}
        monitor = Monitor(0, lambda: state)
        base = f"http://127.0.0.1:{monitor.server.server_port}"
        try:
            with self.assertRaises(HTTPError) as error: urlopen(base + "/health")
            self.assertEqual(error.exception.code, 503)
            state["ready"] = True
            with urlopen(base + "/health") as response:
                self.assertTrue(json.load(response)["ready"])
            with urlopen(base + "/metrics") as response:
                metrics = response.read().decode()
                self.assertIn("petals:rx_bytes_total 37.0", metrics)
                self.assertNotIn("private-id", metrics)
        finally:
            monitor.close()


if __name__ == "__main__": unittest.main()
