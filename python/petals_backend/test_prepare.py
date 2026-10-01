import json
import tempfile
import unittest
from pathlib import Path

from petals_backend.prepare import seal
from petals_backend.artifacts import verify


class PrepareModel(unittest.TestCase):
    def test_seal_actual_safetensors_and_tokenizer(self):
        import torch
        from tokenizers import Tokenizer
        from tokenizers.models import WordLevel
        from transformers import LlamaConfig, LlamaForCausalLM, PreTrainedTokenizerFast

        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            torch.manual_seed(42)
            model = LlamaForCausalLM(LlamaConfig(hidden_size=64, intermediate_size=128,
                                                num_hidden_layers=2, num_attention_heads=4,
                                                num_key_value_heads=4, vocab_size=128,
                                                max_position_embeddings=256))
            model.save_pretrained(root, safe_serialization=True)
            vocab = {"<unk>": 0, "<bos>": 1, "<eos>": 2, **{f"word{i}": i for i in range(3, 128)}}
            tokenizer = PreTrainedTokenizerFast(tokenizer_object=Tokenizer(WordLevel(vocab, unk_token="<unk>")),
                                                unk_token="<unk>", bos_token="<bos>", eos_token="<eos>")
            tokenizer.chat_template = "{% for m in messages %}{{m['content'] + ' '}}{% endfor %}"
            tokenizer.save_pretrained(root)
            kwargs = dict(revision="synthetic-test-v1", tokenizer_revision="synthetic-test-v1", quantization="nf4")
            manifest, checksum = seal(root, **kwargs)
            self.assertEqual(manifest["layers"], 2)
            self.assertGreater(manifest["block_mib"], 0)
            self.assertGreater(manifest["load_ram_mib"], 0)
            self.assertEqual(verify(root, checksum), manifest)
            self.assertEqual(seal(root, **kwargs)[1], checksum)
            with self.assertRaises(ValueError): seal(root, **{**kwargs, "revision": "changed"})
            with self.assertRaises(ValueError): seal(root, **kwargs, template="changed")
            # Do not create an immutable but invalid manifest on missing input.
            (root / "platform-manifest.json").unlink()
            (root / "tokenizer.json").unlink()
            with self.assertRaises(ValueError): seal(root, **kwargs)
            self.assertFalse((root / "platform-manifest.json").exists())


if __name__ == "__main__": unittest.main()
