#!/usr/bin/env python3
"""Demo upstream stubs for scripts/demo.sh.

Starts two OpenAI/Git-shaped HTTP services on 127.0.0.1:
  - :19099 LLM provider (returns one chat.completion)
  - :19098 Git upstream (requires the demo bearer token, returns file JSON)

They exist to demonstrate the platform boundary: the gateway injects the
server-side credential and the employee never sees it.
"""
import json
import logging
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

logging.basicConfig(level=logging.INFO, format="stub: %(message)s")
GIT_TOKEN = "demo-git-secret-token"


class ProviderHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")
        logging.info("provider <- %s %s (Authorization=%s)",
                     body.get("model"), [m.get("content", "")[:40] for m in body.get("messages", [])],
                     self.headers.get("Authorization"))
        resp = {
            "id": "chatcmpl-demo", "object": "chat.completion", "model": body.get("model", "demo"),
            "choices": [{"index": 0, "message": {"role": "assistant",
                         "content": f"Demo 回复：已处理「{body.get('messages', [{}])[-1].get('content', '')[:60]}」"},
                         "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 12, "completion_tokens": 9, "total_tokens": 21},
        }
        payload = json.dumps(resp).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):  # silence default noise
        pass


class GitHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        auth = self.headers.get("Authorization", "")
        logging.info("git <- GET %s (Authorization=%s)", self.path, auth)
        if auth != f"Bearer {GIT_TOKEN}":
            self.send_response(401)
            self.end_headers()
            return
        payload = json.dumps({"repo": "acme/app", "path": "README.md", "content": "# Demo README\n由受治理的 MCP 工具读取。"}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass


def serve(port, handler):
    server = ThreadingHTTPServer(("127.0.0.1", port), handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


if __name__ == "__main__":
    serve(19099, ProviderHandler)
    serve(19098, GitHandler)
    print("stubs ready: provider :19099, git :19098")
    threading.Event().wait()
