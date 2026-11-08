"""A stand-in for Zharp's ingest in CI: accepts every request the agent makes (OTLP metrics, logs,
heartbeats) with 200, and appends each body, un-gzipped, to a file, so a test can look for metric
names in what arrived. Usage: fake-ingest.py <port> <file>"""
import gzip
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

port, out = int(sys.argv[1]), sys.argv[2]


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        if self.headers.get("Content-Encoding") == "gzip":
            try:
                body = gzip.decompress(body)
            except OSError:
                pass
        with open(out, "ab") as f:
            f.write(self.path.encode() + b"\n" + body + b"\n")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b"{}")

    do_PUT = do_POST

    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"{}")

    def log_message(self, *args):
        pass


ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()
