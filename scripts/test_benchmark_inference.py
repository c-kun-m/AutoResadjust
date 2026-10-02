import io
import json
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import benchmark_inference as bench


def event(data):
    return b"data: " + json.dumps(data).encode() + b"\n\n"


class StreamTests(unittest.TestCase):
    def test_role_comments_excluded_actual_usage_not_chunk_count(self):
        stream = b": heartbeat\n\n" + event({"choices": [{"delta": {"role": "assistant"}}]})
        stream += event({"choices": [{"delta": {"content": "several tokens together"}}]})
        stream += event({"choices": [{"delta": {}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 256, "completion_tokens": 7}})
        stream += b"data: [DONE]\n\n"
        clock = iter([12, 14])
        result = bench.read_stream(io.BytesIO(stream), 10, 128, lambda: next(clock))
        self.assertEqual(result["ttft_seconds"], 2)
        self.assertEqual(result["end_to_end_tokens_per_second"], 7 / 4)

    def test_failure_never_accepted_as_success(self):
        prefixes = [b"data: [DONE]\n\n", event({"error": {"message": "private details"}}), b"data: {}\n\n",
                    b"data: " + b"x" * 1048577]
        for stream in prefixes:
            with self.subTest(length=len(stream)), self.assertRaises(bench.StreamError):
                bench.read_stream(io.BytesIO(stream), 0, 128)

    def test_failed_or_wrong_size_samples_cannot_pass_targets(self):
        record = {"endpoint": "local", "warmup": False, "success": True, "ttft_seconds": 1,
                  "prompt_tokens": 256, "end_to_end_tokens_per_second": 10}
        records = [{**record, "warmup": True}] + [dict(record) for _ in range(30)]
        self.assertTrue(bench.summarize(records, "local", 256)["latency_target_met"])
        records[-1]["success"] = False
        self.assertIsNone(bench.summarize(records, "local", 256)["latency_target_met"])
        records[-1]["success"] = True
        records[-1]["prompt_tokens"] = 255
        self.assertFalse(bench.summarize(records, "local", 256)["complete_warmed_sample"])

    def test_total_deadline_interrupts_heartbeat_only_stream(self):
        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"
            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                try:
                    for _ in range(50):
                        self.wfile.write(b": still alive\n\n")
                        self.wfile.flush()
                        time.sleep(.03)
                except OSError:
                    pass
            def log_message(self, *_):
                pass
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            result = bench.request({"name": "stall", "model": "fake", "url": f"http://127.0.0.1:{server.server_port}/v1/chat/completions"},
                                   {"id": "test", "messages": [{"role": "user", "content": "test"}]}, 128, .2, "")
            self.assertFalse(result["success"])
            self.assertLess(result["total_seconds"], 1)
        finally:
            server.shutdown()
            server.server_close()


if __name__ == "__main__":
    unittest.main()
