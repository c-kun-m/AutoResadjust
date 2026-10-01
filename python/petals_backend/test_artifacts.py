import hashlib
import json
import tempfile
import unittest
from pathlib import Path

from petals_backend.artifacts import digest, verify


class ManifestTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        data = {"config.json": json.dumps({"model_type": "llama", "num_hidden_layers": 2, "max_position_embeddings": 512}),
                "tokenizer_config.json": "{}", "tokenizer.json": "{}", "chat_template.jinja": "{{ messages }}",
                "model.safetensors": "test fixture, not real weights"}
        for name, text in data.items():
            (self.root / name).write_text(text)
        self.manifest = {"version": 1, "architecture": "llama", "quantization": "nf4", "dtype": "float16",
                         "revision": "test", "tokenizer_revision": "test", "layers": 2, "context_limit": 512,
                         "files": {name: {"bytes": (self.root / name).stat().st_size, "sha256": digest(self.root / name)} for name in data}}

    def save(self):
        path = self.root / "platform-manifest.json"
        path.write_text(json.dumps(self.manifest))
        return digest(path)

    def test_all_inputs_are_pinned(self):
        checksum = self.save()
        self.assertEqual(verify(self.root, checksum)["layers"], 2)
        (self.root / "chat_template.jinja").write_text("changed")
        with self.assertRaises(ValueError): verify(self.root, checksum)

    def test_no_unverified_alternatives_or_remote_code(self):
        for filename, content in (("pytorch_model.bin", b"pickle"), ("generation_config.json", b"{}")):
            with self.subTest(filename=filename):
                path = self.root / filename
                path.write_bytes(content)
                with self.assertRaises(ValueError): verify(self.root, self.save())
                path.unlink()
        name = "config.json"
        (self.root / name).write_text('{"model_type":"llama","auto_map":{"AutoModel":"custom.Model"}}')
        self.manifest["files"][name] = {"bytes": (self.root/name).stat().st_size, "sha256": digest(self.root/name)}
        with self.assertRaises(ValueError): verify(self.root, self.save())

    def test_manifest_and_index_cannot_escape_directory(self):
        self.manifest["files"]["../escape.safetensors"] = {"bytes": 0, "sha256": hashlib.sha256(b"").hexdigest()}
        with self.assertRaises(ValueError): verify(self.root, self.save())
        del self.manifest["files"]["../escape.safetensors"]
        name = "model.safetensors.index.json"
        (self.root/name).write_text('{"weight_map":{"model.layers.0.weight":"../escape.safetensors"}}')
        self.manifest["files"][name] = {"bytes": (self.root/name).stat().st_size, "sha256": digest(self.root/name)}
        with self.assertRaises(ValueError): verify(self.root, self.save())


if __name__ == "__main__": unittest.main()
