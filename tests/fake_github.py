#!/usr/bin/env python3
"""A fake GitHub API for tests/github-app-token.test. Prints its port, then serves until killed.

Each App is told apart by the `iss` claim of the JWT it presents: the token it is
handed names that App, so a test can tell whose credential minted what.
App 1 is installed on o/r (installation 7); App 2 on o/r and o/q (installations 7 and 8).
"""
import base64
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

INSTALLED = {"1": {7: ["o/r"]}, "2": {7: ["o/r"], 8: ["o/q"]}}


def iss(auth):
    claims = auth.split(" ", 1)[1].split(".")[1]
    return json.loads(base64.urlsafe_b64decode(claims + "=" * (-len(claims) % 4)))["iss"]


class Handler(BaseHTTPRequestHandler):
    def reply(self, code, body):
        raw = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        auth = self.headers["Authorization"]
        path = self.path.split("?")[0]
        if path == "/app/installations":
            app = iss(auth)
            return self.reply(200, [{"id": i} for i in INSTALLED[app]])
        if path.startswith("/repos/") and path.endswith("/installation"):
            repo = path[len("/repos/"):-len("/installation")]
            app = iss(auth)
            for inst, repos in INSTALLED[app].items():
                if repo in repos:
                    return self.reply(200, {"id": inst})
            return self.reply(404, {"message": "Not Found"})
        if path == "/installation/repositories":
            app, inst = auth.split(" ", 1)[1].split("-")[1:3]
            return self.reply(200, {"repositories": [{"full_name": r} for r in INSTALLED[app][int(inst)]]})
        self.reply(500, {"message": "unexpected " + path})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or "null")
        parts = self.path.split("/")
        if len(parts) == 5 and parts[1:3] == ["app", "installations"] and parts[4] == "access_tokens":
            app = iss(self.headers["Authorization"])
            scope = ",".join(body["repositories"]) if body else "all"
            return self.reply(201, {"token": f"tok-{app}-{parts[3]}-{scope}"})
        self.reply(500, {"message": "unexpected " + self.path})

    def log_message(self, *args):
        pass  # silence the per-request log


server = HTTPServer(("127.0.0.1", 0), Handler)
print(server.server_address[1], flush=True)
try:
    server.serve_forever()
except KeyboardInterrupt:
    sys.exit(0)
