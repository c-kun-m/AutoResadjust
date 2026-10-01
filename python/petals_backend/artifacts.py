"""Offline model directory verification. Never execute downloaded model code."""
import hashlib
import json
import re
from pathlib import Path

_NAME = re.compile(r"[A-Za-z0-9_.-]{1,160}\Z")
_HASH = re.compile(r"[0-9a-f]{64}\Z")
_METADATA = {"config.json", "generation_config.json", "tokenizer.json", "tokenizer_config.json",
             "tokenizer.model", "special_tokens_map.json", "added_tokens.json", "chat_template.jinja",
             "model.safetensors.index.json"}


def digest(path):
    value = hashlib.sha256()
    with path.open("rb") as stream:
        while data := stream.read(1 << 20):
            value.update(data)
    return value.hexdigest()


def safe_file(root, name):
    if not isinstance(name, str) or not _NAME.fullmatch(name) or name in (".", ".."):
        raise ValueError("model files must be basenames")
    path = root / name
    # Read-only model mounts must contain real files, not links outside the
    # verified directory or mutable Hugging Face cache references.
    if path.is_symlink() or not path.is_file() or path.resolve().parent != root.resolve():
        raise ValueError(f"model file missing or not a regular local file: {name}")
    limit = 64 << 20 if name in ("tokenizer.json", "tokenizer.model") else 1 << 20
    if not name.endswith(".safetensors") and path.stat().st_size > limit:
        raise ValueError(f"model metadata exceeds its size limit: {name}")
    return path


def verify(root, expected_hash):
    root = Path(root)
    if root.is_symlink() or not root.is_dir() or not _HASH.fullmatch(expected_hash):
        raise ValueError("invalid model directory or manifest digest")
    path = safe_file(root, "platform-manifest.json")
    if path.stat().st_size > 1 << 20 or digest(path) != expected_hash:
        raise ValueError("model manifest digest mismatch")
    manifest = json.loads(path.read_text())
    return validate_manifest(root, manifest)


def validate_manifest(root, manifest):
    root = Path(root)
    if not isinstance(manifest, dict):
        raise ValueError("manifest must be a JSON object")
    if manifest.get("version") != 1 or manifest.get("architecture") != "llama":
        raise ValueError("only version 1 Llama manifests are supported")
    if manifest.get("quantization") not in ("none", "int8", "nf4") or manifest.get("dtype") != "float16":
        raise ValueError("unsupported execution precision")
    if not manifest.get("revision") or not manifest.get("tokenizer_revision"):
        raise ValueError("immutable model and tokenizer revisions are required")
    files = manifest.get("files")
    if not isinstance(files, dict) or not 3 <= len(files) <= 512:
        raise ValueError("manifest must list 3..512 files")
    if not {"config.json", "tokenizer_config.json", "chat_template.jinja"}.issubset(files):
        raise ValueError("model config, tokenizer config and explicit chat template are required")
    if not ("tokenizer.json" in files or "tokenizer.model" in files):
        raise ValueError("tokenizer payload is missing")
    if not any(name.endswith(".safetensors") for name in files):
        raise ValueError("safetensors weights are missing")
    for name, entry in files.items():
        if name not in _METADATA and not name.endswith(".safetensors"):
            raise ValueError("unsupported model file; pickle and remote code are not allowed")
        file = safe_file(root, name)
        if not isinstance(entry, dict) or not _HASH.fullmatch(entry.get("sha256", "")):
            raise ValueError(f"invalid hash for {name}")
        if file.stat().st_size != entry.get("bytes") or digest(file) != entry["sha256"]:
            raise ValueError(f"model file digest mismatch: {name}")
    # Libraries may select optional config/tokenizer files automatically; all
    # such files must be covered by the manifest rather than silently preferred.
    for file in root.iterdir():
        if (file.name in _METADATA or file.name.endswith((".safetensors", ".bin", ".py"))) and file.name not in files:
            raise ValueError(f"unverified model input: {file.name}")
    config = json.loads((root / "config.json").read_text())
    tokenizer = json.loads((root / "tokenizer_config.json").read_text())
    if config.get("model_type") != "llama" or config.get("auto_map") or tokenizer.get("auto_map"):
        raise ValueError("remote model/tokenizer code is not supported")
    if config.get("num_hidden_layers") != manifest.get("layers") or not 1 <= manifest["layers"] <= 1024:
        raise ValueError("layer count conflicts with model config")
    if not 128 <= manifest.get("context_limit", 0) <= config.get("max_position_embeddings", 0):
        raise ValueError("context limit conflicts with model config")
    template = (root / "chat_template.jinja").read_text()
    if not template.strip() or len(template.encode()) > 65536:
        raise ValueError("chat template must contain 1..65536 bytes")
    index = root / "model.safetensors.index.json"
    if index.exists():
        mapping = json.loads(index.read_text()).get("weight_map", {})
        if not mapping or any(name not in files or not name.endswith(".safetensors") for name in mapping.values()):
            raise ValueError("weight index references an unverified shard")
    return manifest
