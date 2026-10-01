"""Seal an already downloaded Hugging Face safetensors directory for offline use.

This command never downloads weights and never overwrites an existing manifest.
It emits catalog metadata for the upcoming Petals platform integration.
"""
import argparse
import json
import math
import re
from pathlib import Path

from .artifacts import _METADATA, digest, safe_file, verify, validate_manifest


def seal(root, *, revision, tokenizer_revision, quantization, template=None):
    root = Path(root)
    if root.is_symlink() or not root.is_dir():
        raise ValueError("model directory must be a regular directory")
    manifest_path = root / "platform-manifest.json"
    if manifest_path.exists():
        manifest = verify(root, digest(manifest_path))
        if (manifest["revision"], manifest["tokenizer_revision"], manifest["quantization"]) != (revision, tokenizer_revision, quantization):
            raise ValueError("existing manifest is immutable; prepare a new directory")
        if template is not None and (root / "chat_template.jinja").read_text() != template:
            raise ValueError("chat template conflicts with immutable manifest")
        return manifest, digest(manifest_path)
    if not revision or not tokenizer_revision or len(revision) > 160 or len(tokenizer_revision) > 160:
        raise ValueError("explicit model/tokenizer revisions are required")
    config_data = json.loads(safe_file(root, "config.json").read_text())
    token_data = json.loads(safe_file(root, "tokenizer_config.json").read_text())
    if config_data.get("model_type") != "llama" or config_data.get("auto_map") or token_data.get("auto_map"):
        raise ValueError("only standard Llama models and tokenizers are supported")
    if template is None:
        existing = root / "chat_template.jinja"
        template = safe_file(root, "chat_template.jinja").read_text() if existing.exists() else token_data.get("chat_template")
    if not isinstance(template, str) or not template.strip() or len(template.encode()) > 65536:
        raise ValueError("provide an explicit chat template (maximum 64 KiB)")
    template_path = root / "chat_template.jinja"
    if template_path.exists():
        if safe_file(root, "chat_template.jinja").read_text() != template:
            raise ValueError("existing chat template differs; use a new directory")
    else:
        template_path.write_text(template)
    files = {}
    for path in sorted(root.iterdir()):
        if path.name in _METADATA or path.name.endswith(".safetensors"):
            safe_file(root, path.name)
            files[path.name] = {"bytes": path.stat().st_size, "sha256": digest(path)}
        elif path.name.endswith((".bin", ".py")):
            raise ValueError("pickle checkpoints and remote model code are not supported")
    weights = [entry["bytes"] for name, entry in files.items() if name.endswith(".safetensors")]
    if not weights:
        raise ValueError("safetensors weights are missing")

    import torch
    from petals.models.llama.config import DistributedLlamaConfig
    from petals.server.block_utils import get_block_size
    from petals.utils.convert_block import QuantType
    config = DistributedLlamaConfig.from_pretrained(str(root), local_files_only=True, initial_peers=[])
    size = get_block_size(config, "memory", dtype=torch.float16, quant_type=QuantType[quantization.upper()])
    # Conservative planning estimates, not observed device/memory usage. Engine
    # loading and a real end-to-end warmup remain mandatory admission gates.
    block_mib = max(1, math.ceil(size * 1.15 / 1048576))
    embeddings_bytes = int(config.vocab_size) * int(config.hidden_size) * 4 * 2
    load_ram_mib = math.ceil((2 * max(weights) + embeddings_bytes) / 1048576) + 1024
    manifest = {"version": 1, "architecture": "llama", "revision": revision,
                "tokenizer_revision": tokenizer_revision, "quantization": quantization, "dtype": "float16",
                "layers": int(config.num_hidden_layers), "context_limit": min(131072, int(config.max_position_embeddings)),
                "weight_mib": math.ceil(sum(weights) / 1048576), "block_mib": block_mib,
                "load_ram_mib": load_ram_mib, "files": files}
    validate_manifest(root, manifest)
    # Exclusive creation protects an existing sealed model from accidental edits.
    with manifest_path.open("x") as stream:
        json.dump(manifest, stream, sort_keys=True, separators=(",", ":"))
    checksum = digest(manifest_path)
    verify(root, checksum)
    return manifest, checksum


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--model-dir", required=True, type=Path)
    parser.add_argument("--id", required=True)
    parser.add_argument("--name", required=True)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--tokenizer-revision", required=True)
    parser.add_argument("--quantization", choices=("none", "int8", "nf4"), required=True)
    parser.add_argument("--chat-template", type=Path)
    args = parser.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9_-][A-Za-z0-9_.-]{0,119}", args.id) or not re.fullmatch(r"[A-Za-z0-9_-][A-Za-z0-9_.-]{0,119}", args.model_dir.name):
        parser.error("id and model directory basename must be catalog identifiers")
    manifest, checksum = seal(args.model_dir, revision=args.revision, tokenizer_revision=args.tokenizer_revision,
                              quantization=args.quantization, template=args.chat_template.read_text() if args.chat_template else None)
    artifact = {"id": args.id, "name": args.name, "format": "safetensors", "file": args.model_dir.name,
                "sha256": checksum, **{key: manifest[key] for key in (
                    "revision", "tokenizer_revision", "architecture", "quantization", "layers", "context_limit", "weight_mib", "block_mib", "load_ram_mib")}}
    print(json.dumps(artifact, indent=2))


if __name__ == "__main__": main()
