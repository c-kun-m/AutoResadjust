"""Bounded loopback monitoring; inference counters are protobuf payload bytes."""
import json
import multiprocessing as mp
import threading
from pathlib import Path
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Counters:
    NAMES = ("sessions_total", "active_sessions", "errors_total", "rx_bytes_total",
             "tx_bytes_total", "steps_total", "duration_seconds_total")

    def __init__(self):
        self.values = mp.Array("d", len(self.NAMES))

    def add(self, **values):
        with self.values.get_lock():
            for name, value in values.items():
                self.values[self.NAMES.index(name)] += value

    def snapshot(self):
        with self.values.get_lock():
            return dict(zip(self.NAMES, self.values[:]))


class BoundedHTTPServer(ThreadingHTTPServer):
    daemon_threads = True
    request_queue_size = 16

    def __init__(self, *args, **kwargs):
        self.slots = threading.BoundedSemaphore(16)
        super().__init__(*args, **kwargs)

    def process_request(self, request, address):
        request.settimeout(3)
        if not self.slots.acquire(blocking=False):
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, address)
        except BaseException:
            self.slots.release()
            raise

    def process_request_thread(self, request, address):
        try:
            super().process_request_thread(request, address)
        finally:
            self.slots.release()


def container_traffic():
    """Linux network namespace counters; missing interfaces stay unknown."""
    try:
        values = []
        for line in Path("/proc/net/dev").read_text().splitlines()[2:]:
            name, fields = line.split(":", 1)
            fields = fields.split()
            if name.strip() != "lo" and len(fields) >= 16:
                values.append((int(fields[0]), int(fields[8])))
        if values:
            return {"traffic_source": "container-network", "receive_bytes_total": sum(v[0] for v in values),
                    "transmit_bytes_total": sum(v[1] for v in values)}
    except (OSError, ValueError):
        pass
    return {}


class Monitor:
    def __init__(self, port, snapshot, host="127.0.0.1"):
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                if self.path not in ("/health", "/metrics"):
                    self.send_error(404)
                    return
                data = snapshot()
                status, content_type = 200, "application/json"
                if self.path == "/health":
                    status = 200 if data.get("ready") else 503
                    body = json.dumps(data).encode()
                else:
                    content_type = "text/plain; version=0.0.4"
                    body = "".join(f"petals:{key} {float(value)}\n" for key, value in data.items()
                                   if isinstance(value, (bool, int, float))).encode()
                self.send_response(status)
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *_):
                pass

        self.server = BoundedHTTPServer((host, port), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
