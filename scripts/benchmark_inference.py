"""Measure existing platform chat endpoints; never provision or simulate GPUs.

Python 3.10+, standard library only. Tokens come exclusively from server usage.
Credentials come from an environment variable; prompts/responses are not saved.
"""
import argparse
import contextlib
import hashlib
import http.client
import json
import math
import os
import socket
import statistics
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlsplit


class StreamError(ValueError):
    pass


def read_stream(response, started, max_tokens, clock=time.monotonic):
    first, finished, usage = None, False, None
    event, size, event_size = [], 0, 0
    while True:
        line = response.readline(1048577)
        size += len(line)
        event_size += len(line)
        if len(line) > 1048576 or event_size > 1048576 or size > 8388608:
            raise StreamError("stream_size_limit")
        if not line:
            raise StreamError("stream_ended_without_done")
        if line.strip():
            if line.startswith(b"data:"):
                event.append(line[5:].strip())
            continue
        payload = b"\n".join(event)
        event, event_size = [], 0
        if not payload:
            continue  # comments, role events and headers never count as TTFT
        if payload == b"[DONE]":
            if not finished or first is None:
                raise StreamError("missing_output_or_finish")
            if not isinstance(usage, dict):
                raise StreamError("missing_actual_usage")
            prompt, completion = usage.get("prompt_tokens"), usage.get("completion_tokens")
            if type(prompt) is not int or prompt < 1 or type(completion) is not int or not 1 <= completion <= max_tokens:
                raise StreamError("invalid_actual_usage")
            elapsed = clock() - started
            return {"ttft_seconds": first - started, "total_seconds": elapsed,
                    "prompt_tokens": prompt, "completion_tokens": completion,
                    # Conservative user-visible throughput includes prefill,
                    # queueing, network and final usage. SSE chunks are not tokens.
                    "end_to_end_tokens_per_second": completion / elapsed}
        try:
            data = json.loads(payload)
            if not isinstance(data, dict) or data.get("error"):
                raise StreamError("upstream_stream_error")
            if data.get("usage") is not None:
                usage = data["usage"]
            for choice in data.get("choices", []):
                delta = choice.get("delta") or {}
                if any(delta.get(key) for key in ("content", "reasoning_content", "refusal", "tool_calls")):
                    if finished:
                        raise StreamError("output_after_finish")
                    if first is None:
                        first = clock()
                if choice.get("finish_reason") is not None:
                    finished = True
        except (TypeError, AttributeError, json.JSONDecodeError) as error:
            raise StreamError("invalid_stream_event") from error


def request(endpoint, case, max_tokens, timeout, token):
    url = urlsplit(endpoint["url"])
    connection_type = http.client.HTTPSConnection if url.scheme == "https" else http.client.HTTPConnection
    connection = connection_type(url.hostname, url.port, timeout=min(10, timeout))
    started, timer = time.monotonic(), None
    record = {"endpoint": endpoint["name"], "case": case["id"], "success": False}
    try:
        connection.connect()
        sock = connection.sock
        def expire():
            with contextlib.suppress(OSError):
                sock.shutdown(socket.SHUT_RDWR)
        remaining = timeout - (time.monotonic() - started)
        if remaining <= 0:
            raise TimeoutError()
        sock.settimeout(remaining)
        timer = threading.Timer(remaining, expire)
        timer.daemon = True
        timer.start()
        headers = {"Content-Type": "application/json"}
        if token:
            headers["Authorization"] = "Bearer " + token
        body = {"model": endpoint["model"], "messages": case["messages"], "stream": True,
                "stream_options": {"include_usage": True}, "max_tokens": max_tokens, "temperature": 0}
        connection.request("POST", url.path or "/v1/chat/completions", json.dumps(body).encode(), headers)
        response = connection.getresponse()
        record["http_status"] = response.status
        record["request_id"] = response.getheader("X-Request-ID", "")[:128]
        if response.status != 200:
            raise StreamError("http_error")
        if not response.getheader("Content-Type", "").startswith("text/event-stream"):
            raise StreamError("not_an_event_stream")
        record.update(read_stream(response, started, max_tokens))
        record["success"] = True
    except (OSError, ValueError, http.client.HTTPException) as error:
        record["error"] = str(error) if isinstance(error, StreamError) else type(error).__name__
        record["total_seconds"] = time.monotonic() - started
    finally:
        if timer:
            timer.cancel()
        connection.close()
    return record


def summarize(records, name, input_tokens):
    measured = [r for r in records if r["endpoint"] == name and not r["warmup"]]
    good = [r for r in measured if r["success"]]
    result = {"requests": len(measured), "completed": len(good), "failed": len(measured) - len(good)}
    if not good:
        return result
    latencies = sorted(r["ttft_seconds"] for r in good)
    p95 = latencies[math.ceil(.95 * len(latencies)) - 1]
    rate = statistics.median(r["end_to_end_tokens_per_second"] for r in good)
    matched = all(r["prompt_tokens"] == input_tokens for r in good)
    warm = [r for r in records if r["endpoint"] == name and r["warmup"]]
    valid = len(good) >= 30 and len(good) == len(measured) and matched and bool(warm) and all(r["success"] for r in warm)
    result.update(ttft_p95_seconds=p95, median_end_to_end_tokens_per_second=rate,
                  actual_input_tokens_match=matched, complete_warmed_sample=valid,
                  latency_target_met=(p95 <= 5) if valid else None,
                  conservative_throughput_target_met=(rate >= 5) if valid else None)
    return result


def validate(config):
    endpoints, cases = config.get("endpoints"), config.get("cases")
    if not isinstance(endpoints, list) or not 1 <= len(endpoints) <= 3 or not isinstance(cases, list) or not 1 <= len(cases) <= 128:
        raise ValueError("1..3 endpoints and 1..128 cases required")
    if len({e["name"] for e in endpoints}) != len(endpoints) or len({c["id"] for c in cases}) != len(cases):
        raise ValueError("endpoint names and case IDs must be unique")
    for endpoint in endpoints:
        allowed = {"name", "url", "model", "backend", "model_revision", "tokenizer_revision", "engine_version", "artifact_sha256", "precision"}
        if set(endpoint) != allowed:
            raise ValueError("endpoint metadata must use exactly the documented fields; credentials belong in the environment")
        url = urlsplit(endpoint["url"])
        if url.scheme not in ("http", "https") or not url.hostname or url.username or url.password or url.query or url.fragment:
            raise ValueError("HTTP(S) endpoint without credentials/query/fragment required")
        if endpoint["backend"] not in ("llama_rpc", "llama_local", "petals") or not endpoint["model"]:
            raise ValueError("supported backend and model pool ID required")
        for field in ("model_revision", "tokenizer_revision", "engine_version", "artifact_sha256", "precision"):
            if not isinstance(endpoint.get(field), str) or not endpoint[field]:
                raise ValueError(f"record pinned {field} for each endpoint")
        digest = endpoint["artifact_sha256"]
        if len(digest) != 64 or any(c not in "0123456789abcdef" for c in digest):
            raise ValueError("artifact_sha256 must be a lowercase SHA-256 digest")
    for field in ("model_revision", "tokenizer_revision"):
        if len({e[field] for e in endpoints}) != 1:
            raise ValueError(f"comparisons require the same {field}")
    for case in cases:
        if not isinstance(case.get("messages"), list) or not case["messages"]:
            raise ValueError("nonempty messages required")
        for message in case["messages"]:
            if set(message) != {"role", "content"} or message["role"] not in ("system", "user", "assistant") or not isinstance(message["content"], str):
                raise ValueError("text chat messages required")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("config", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--requests", type=int, default=30, help="measured requests per endpoint")
    parser.add_argument("--warmup", type=int, default=2)
    parser.add_argument("--input-tokens", type=int, default=256)
    parser.add_argument("--max-tokens", type=int, default=128)
    parser.add_argument("--timeout", type=float, default=130)
    parser.add_argument("--token-env", default="CONTROL_PLANE_TOKEN")
    args = parser.parse_args()
    if not 1 <= args.requests <= 10000 or not 1 <= args.warmup <= 100 or not 1 <= args.max_tokens <= 128 or not 1 <= args.input_tokens <= 131072 or not math.isfinite(args.timeout) or not 1 <= args.timeout <= 600:
        parser.error("invalid bounded benchmark settings")
    raw = args.config.read_bytes()
    config = json.loads(raw)
    validate(config)
    if args.output.resolve() == args.config.resolve() or args.output.exists():
        parser.error("output must be a new file")
    records = []
    report = {"created_at": datetime.now(timezone.utc).isoformat(), "config_sha256": hashlib.sha256(raw).hexdigest(),
              "scope": "client measurements; physical host count, 13B model and resource inventory require separate verification",
              "throughput_definition": "actual completion tokens / total request seconds, including queue, prefill and network",
              "target_input_tokens": args.input_tokens, "output_token_limit": args.max_tokens,
              "declared_endpoints": [{k: v for k, v in e.items() if k not in ("url",)} for e in config["endpoints"]],
              "requests": records}
    args.output.parent.mkdir(parents=True, exist_ok=True)
    # Reserve without clobbering an existing evidence file.
    with args.output.open("x", encoding="utf-8") as output:
        json.dump(report, output, indent=2)
    try:
        for index in range(args.warmup + args.requests):
            for endpoint in config["endpoints"]:
                case = config["cases"][index % len(config["cases"])]
                record = request(endpoint, case, args.max_tokens, args.timeout, os.getenv(args.token_env, ""))
                record.update(warmup=index < args.warmup, index=index)
                records.append(record)
                report["summary"] = {e["name"]: summarize(records, e["name"], args.input_tokens) for e in config["endpoints"]}
                temporary = args.output.with_suffix(args.output.suffix + ".tmp")
                temporary.write_text(json.dumps(report, indent=2), encoding="utf-8")
                temporary.replace(args.output)
                print(json.dumps(record), flush=True)
    except KeyboardInterrupt:
        raise SystemExit("Interrupted; completed measurements retained")
    print(json.dumps(report["summary"], indent=2))
    if any(not r["success"] for r in records):
        raise SystemExit(1)


if __name__ == "__main__":
    main()
