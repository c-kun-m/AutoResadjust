"""Private Llama chat gateway with one owned, cancellable inference session."""
import os
os.environ["HF_HUB_OFFLINE"] = "1"
os.environ["TRANSFORMERS_OFFLINE"] = "1"

import argparse
import json
import math
import queue
import signal
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler
from pathlib import Path

from .artifacts import verify, verify_assignment_metadata
from .monitor import BoundedHTTPServer
from .network import private_dht
from .runtime_config import PROFILE, integer, validate


def chat_request(body, context_size):
    if not isinstance(body, dict):
        raise ValueError("request must be a JSON object")
    supported = {"model", "messages", "max_tokens", "max_completion_tokens", "temperature", "top_p", "stream", "stream_options", "n"}
    if set(body) - supported:
        raise ValueError("unsupported chat parameters: " + ", ".join(sorted(set(body) - supported)))
    if body.get("n", 1) != 1 or type(body.get("stream", False)) is not bool:
        raise ValueError("n=1 and a boolean stream option are required")
    if "max_tokens" in body and "max_completion_tokens" in body:
        raise ValueError("choose one output token limit")
    count = integer(body.get("max_tokens", body.get("max_completion_tokens", 128)), 1, min(4096, context_size - 1), "max_tokens")
    temperature, top_p = body.get("temperature", .7), body.get("top_p", 1.0)
    for value, low, high, name in ((temperature, 0, 2, "temperature"), (top_p, .001, 1, "top_p")):
        if type(value) not in (int, float) or not math.isfinite(value) or not low <= value <= high:
            raise ValueError(f"invalid {name}")
    messages = body.get("messages")
    if not isinstance(messages, list) or not 1 <= len(messages) <= 256:
        raise ValueError("1..256 text messages are required")
    for message in messages:
        if (not isinstance(message, dict) or set(message) != {"role", "content"}
                or message["role"] not in ("user", "assistant", "system") or not isinstance(message["content"], str)):
            raise ValueError("only user/assistant/system text messages are supported")
    options = body.get("stream_options", {})
    if not isinstance(options, dict) or set(options) - {"include_usage"} or type(options.get("include_usage", False)) is not bool:
        raise ValueError("invalid stream_options")
    return messages, count, float(temperature), float(top_p)


class Cancelled(Exception):
    pass


class Job:
    def __init__(self):
        self.events = queue.Queue(maxsize=32)
        self.cancel = threading.Event()
        self.done = threading.Event()
        self.prompt_tokens = self.completion_tokens = 0
        self.error = None
        self.finish_reason = "length"
        self.began = time.monotonic()
        self.first_token = None
        self.thread = None

    def emit(self, text):
        if not text:
            return
        while not self.cancel.is_set():
            if time.monotonic() - self.began > 120:
                self.cancel.set()
                break
            try:
                self.events.put(text, timeout=.1)
                return
            except queue.Full:
                pass
        raise Cancelled()

    def usage(self):
        return {"prompt_tokens": self.prompt_tokens, "completion_tokens": self.completion_tokens,
                "total_tokens": self.prompt_tokens + self.completion_tokens}


class Engine:
    def __init__(self, config):
        self.config = validate(config, "gateway")
        self.model = self.tokenizer = self.dht = None
        self.slot = threading.Lock()
        self.lock = threading.Lock()
        self.stopping = threading.Event()
        self.current = None
        self.coverage_at = 0
        self.stats = {"requests_total": 0, "completed_total": 0, "failed_total": 0, "canceled_total": 0,
                      "prompt_tokens_total": 0, "tokens_predicted_total": 0, "active_requests": 0,
                      "rx_bytes_total": 0, "tx_bytes_total": 0}

    def load(self):
        from transformers import AutoTokenizer, GenerationConfig
        from petals.models.llama.config import DistributedLlamaConfig
        from .private_model import PrivateLlama
        import torch
        config = self.config
        root = Path(config["model_dir"])
        manifest = verify(root, config["manifest_sha256"])
        verify_assignment_metadata(manifest, config)
        if config["placements"][-1]["end_block"] != manifest["layers"] or config["context_size"] > manifest["context_limit"]:
            raise ValueError("assignments must cover exactly all sealed blocks and fit context")
        self.tokenizer = AutoTokenizer.from_pretrained(root, local_files_only=True, trust_remote_code=False)
        self.tokenizer.chat_template = (root / "chat_template.jinja").read_text()
        self.dht = private_dht(config["initial_peers"], client_mode=True,
                               host_maddrs=["/ip4/127.0.0.1/tcp/0"], start=True)
        model_config = DistributedLlamaConfig.from_pretrained(
            root, local_files_only=True, initial_peers=config["initial_peers"], dht_prefix=config["prefix"],
            allowed_servers=[p["peer_id"] for p in config["placements"]],
            max_retries=1, connect_timeout=3, request_timeout=5, update_period=2,
            # Sequential client RPC makes cancellation and per-worker payload
            # counters unambiguous; speculative peer pushes remain disabled.
            use_server_to_server=False, show_route=False,
        )
        self.model = PrivateLlama.from_pretrained(root, config=model_config, torch_dtype=torch.float32,
                                                  local_files_only=True, dht=self.dht).eval()
        # Use the pinned model's EOS identity, but no hidden sampling/beam
        # defaults that conflict with the public request contract.
        self.model.generation_config = GenerationConfig.from_model_config(model_config)

    def refresh_coverage(self):
        from petals.data_structures import ServerState
        from petals.utils.dht import get_remote_module_infos
        if self.model is None:
            return
        peers = [p["peer_id"] for p in self.config["placements"] for _ in range(p["start_block"], p["end_block"])]
        infos = get_remote_module_infos(self.dht, [f"{self.config['prefix']}.{i}" for i in range(len(peers))], latest=True)
        covered = all(info and any(str(peer) == peers[i] and server.state == ServerState.ONLINE
                                  for peer, server in info.servers.items()) for i, info in enumerate(infos))
        with self.lock:
            self.coverage_at = time.monotonic() if covered else 0

    def snapshot(self):
        with self.lock:
            return {**self.stats, "profile": PROFILE,
                    "ready": self.model is not None and time.monotonic() - self.coverage_at < 6 and not self.stopping.is_set()}

    def add(self, **values):
        with self.lock:
            for name, value in values.items():
                self.stats[name] += value

    def start(self, body):
        messages, count, temperature, top_p = chat_request(body, self.config["context_size"])
        if not self.snapshot()["ready"]:
            raise RuntimeError("assigned blocks are not ready")
        if not self.slot.acquire(blocking=False):
            raise BlockingIOError("previous inference session is still active or canceling")
        try:
            inputs = self.tokenizer.apply_chat_template(messages, add_generation_prompt=True, return_tensors="pt")
            if inputs.shape[1] < 1 or inputs.shape[1] + count > self.config["context_size"]:
                raise ValueError("prompt plus output exceeds the reserved context")
            job = Job()
            job.prompt_tokens = inputs.shape[1]
            self.current = job
            self.add(requests_total=1, active_requests=1)
            job.thread = threading.Thread(target=self.generate, args=(job, inputs, count, temperature, top_p), daemon=True)
            job.thread.start()
            return job
        except BaseException:
            self.slot.release()
            raise

    def generate(self, job, inputs, count, temperature, top_p):
        import torch
        from transformers import TextStreamer, StoppingCriteria, StoppingCriteriaList

        class Stop(StoppingCriteria):
            def __call__(self, *_args, **_kwargs):
                if time.monotonic() - job.began > 120:
                    job.cancel.set()
                return job.cancel.is_set()

        class Streamer(TextStreamer):
            def put(self, value):
                if not self.next_tokens_are_prompt:
                    job.completion_tokens += value.numel()
                    if job.first_token is None:
                        job.first_token = time.monotonic()
                if job.cancel.is_set():
                    raise Cancelled()
                super().put(value)

            def on_finalized_text(self, text, stream_end=False):
                job.emit(text)

        try:
            kwargs = dict(max_new_tokens=count, do_sample=temperature > 0, num_beams=1,
                          streamer=Streamer(self.tokenizer, skip_prompt=True, skip_special_tokens=True),
                          stopping_criteria=StoppingCriteriaList([Stop()]),
                          pad_token_id=self.tokenizer.pad_token_id or self.tokenizer.eos_token_id or 0)
            if temperature > 0:
                kwargs.update(temperature=temperature, top_p=top_p)
            with torch.inference_mode():
                output = self.model.generate(inputs, **kwargs)
            actual_count = output.shape[1] - inputs.shape[1]
            if actual_count != job.completion_tokens:
                raise RuntimeError("token stream and final sequence differ")
            eos = self.model.generation_config.eos_token_id
            eos = [eos] if isinstance(eos, int) else (eos or [])
            job.finish_reason = "stop" if output[0, -1].item() in eos else "length"
        except BaseException as error:
            job.error = type(error).__name__
        finally:
            # This runs after the Petals session context has released its KV
            # ownership. HTTP disconnect alone MUST NOT free the local slot.
            outcome = "canceled" if job.cancel.is_set() else ("failed" if job.error else "completed")
            self.add(**{outcome + "_total": 1}, active_requests=-1,
                     prompt_tokens_total=job.prompt_tokens, tokens_predicted_total=job.completion_tokens)
            job.done.set()
            self.slot.release()

    def cancel_job(self, job):
        if job.done.is_set():
            return
        job.cancel.set()
        job.thread.join(timeout=12)
        if not job.done.is_set():
            # The supervisor restarts the owned process; don't admit overlapping
            # sessions when a native/RPC operation ignored its bounded timeout.
            print(json.dumps({"event": "generation_cancel_stalled", "deployment_id": self.config["deployment_id"]}), flush=True)
            os._exit(1)

    def close(self):
        self.stopping.set()
        if self.current is not None:
            self.cancel_job(self.current)
        if self.model is not None:
            self.model.close()
        if self.dht is not None:
            self.dht.shutdown()


def handler(engine):
    class Handler(BaseHTTPRequestHandler):
        def json_response(self, status, body, count_traffic=True):
            payload = json.dumps(body, ensure_ascii=False).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            if count_traffic:
                engine.add(tx_bytes_total=len(payload))

        def do_GET(self):
            state = engine.snapshot()
            if self.path == "/health":
                self.json_response(200 if state["ready"] else 503, state, count_traffic=False)
            elif self.path == "/metrics":
                payload = "".join(f"petals:{k} {float(v)}\n" for k, v in state.items() if isinstance(v, (int, float, bool))).encode()
                self.send_response(200)
                self.send_header("Content-Type", "text/plain; version=0.0.4")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)
            else:
                self.send_error(404)

        def sse(self, data):
            payload = ("data: " + (data if isinstance(data, str) else json.dumps(data, ensure_ascii=False)) + "\n\n").encode()
            self.wfile.write(payload)
            self.wfile.flush()
            engine.add(tx_bytes_total=len(payload))

        def do_POST(self):
            if self.path != "/v1/chat/completions":
                self.send_error(404)
                return
            job, streamed = None, False
            request_id = self.headers.get("X-Request-ID", "")
            request_id = request_id if len(request_id) <= 128 and request_id.isascii() and request_id.isalnum() else uuid.uuid4().hex
            try:
                size = int(self.headers.get("Content-Length", "0"))
                if not 0 < size <= 2 << 20 or self.headers.get("Transfer-Encoding"):
                    raise ValueError("bounded Content-Length required")
                raw = self.rfile.read(size)
                if len(raw) != size:
                    raise ValueError("incomplete request")
                engine.add(rx_bytes_total=len(raw))
                body = json.loads(raw)
                job = engine.start(body)
                chunk_id = "chatcmpl-" + uuid.uuid4().hex
                base = {"id": chunk_id, "created": int(time.time()), "model": engine.config["deployment_id"]}
                parts = []
                if body.get("stream", False):
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.send_header("Cache-Control", "no-cache")
                    self.send_header("Connection", "close")
                    self.end_headers()
                    streamed = True
                    self.sse({**base, "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": {"role": "assistant"}, "finish_reason": None}]})
                while not job.done.is_set() or not job.events.empty():
                    if time.monotonic() - job.began > 120:
                        raise TimeoutError("generation deadline exceeded")
                    try:
                        text = job.events.get(timeout=.25)
                    except queue.Empty:
                        if streamed:
                            # Detect disconnected clients even during prefill.
                            self.wfile.write(b": waiting\n\n")
                            self.wfile.flush()
                            engine.add(tx_bytes_total=11)
                        continue
                    if streamed:
                        self.sse({**base, "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": {"content": text}, "finish_reason": None}]})
                    else:
                        parts.append(text)
                if job.error or job.cancel.is_set():
                    raise RuntimeError("inference failed: " + (job.error or "canceled"))
                if streamed:
                    self.sse({**base, "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": {}, "finish_reason": job.finish_reason}]})
                    self.sse({**base, "object": "chat.completion.chunk", "choices": [], "usage": job.usage()})
                    self.sse("[DONE]")
                else:
                    self.json_response(200, {**base, "object": "chat.completion", "choices": [{"index": 0,
                        "message": {"role": "assistant", "content": "".join(parts)}, "finish_reason": job.finish_reason}], "usage": job.usage()})
            except (BrokenPipeError, ConnectionResetError):
                pass
            except Exception as error:
                status = 429 if isinstance(error, BlockingIOError) else (400 if isinstance(error, (ValueError, TypeError)) else 503)
                failure = {"error": {"message": str(error), "type": "inference_error"}}
                try:
                    self.sse(failure) if streamed else self.json_response(status, failure)
                except OSError:
                    pass
            finally:
                if job is not None:
                    engine.cancel_job(job)
                    print(json.dumps({"event": "petals_request_finished", "request_id": request_id,
                                      "deployment_id": engine.config["deployment_id"],
                                      "outcome": "canceled" if job.cancel.is_set() else ("failed" if job.error else "completed"),
                                      "duration_seconds": round(time.monotonic() - job.began, 4), **job.usage()}), flush=True)

        def log_message(self, *_):
            pass
    return Handler


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, required=True)
    args = parser.parse_args()
    if args.config.stat().st_size > 65536:
        raise ValueError("configuration too large")
    engine = Engine(json.loads(args.config.read_text()))
    server = BoundedHTTPServer(("127.0.0.1", engine.config["http_port"]), handler(engine))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    signal.signal(signal.SIGTERM, lambda *_: engine.stopping.set())
    signal.signal(signal.SIGINT, lambda *_: engine.stopping.set())
    try:
        engine.load()
        while not engine.stopping.is_set():
            try:
                engine.refresh_coverage()
            except Exception:
                with engine.lock:
                    engine.coverage_at = 0
            engine.stopping.wait(2)
    finally:
        engine.close()
        server.shutdown()
        server.server_close()


if __name__ == "__main__":
    main()
