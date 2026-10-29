#!/usr/bin/env python3
"""A fake GitHub API for tests/github-app-token.test. Prints its port, then serves until killed.

Each App is told apart by the `iss` claim of the JWT it presents: the token it is
handed names that App, so a test can tell whose credential minted what.
App 1 is installed on o/r (installation 7); App 2 on o/r and o/q (installations 7 and 8).
Apps 3 to 5 are installed past a ceiling of the listing the tool reads: App 3's
installation listing is full on its last allowed page, App 4's one installation has
a full repository listing on its last allowed page, and App 5 has more installations
than the tool lists at all.
"""
import base64
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlsplit

# The ceilings roles/github_app/files/github-app-token applies to a listing.
PAGE_SIZE = 100
MAX_PAGES = 10
MAX_INSTALLATIONS = 50

INSTALLED = {
    "1": {7: ["o/r"]},
    "2": {7: ["o/r"], 8: ["o/q"]},
    "3": {i: ["o/paged"] for i in range(PAGE_SIZE * MAX_PAGES)},
    "4": {9: [f"o/wide-{i}" for i in range(PAGE_SIZE * MAX_PAGES)]},
    "5": {i: ["o/crowded"] for i in range(MAX_INSTALLATIONS + 1)},
}


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

    def page(self, items):
        """The slice of items one page asks for; a listing that ends asks for a short page."""
        query = parse_qs(urlsplit(self.path).query)
        size = int(query.get("per_page", [PAGE_SIZE])[0])
        start = (int(query.get("page", ["1"])[0]) - 1) * size
        return items[start:start + size]

    def do_GET(self):
        auth = self.headers["Authorization"]
        path = self.path.split("?")[0]
        if path == "/app/installations":
            app = iss(auth)
            return self.reply(200, [{"id": i} for i in self.page(list(INSTALLED[app]))])
        if path.startswith("/repos/") and path.endswith("/installation"):
            repo = path[len("/repos/"):-len("/installation")]
            app = iss(auth)
            for inst, repos in INSTALLED[app].items():
                if repo in repos:
                    return self.reply(200, {"id": inst})
            return self.reply(404, {"message": "Not Found"})
        if path == "/installation/repositories":
            app, inst = auth.split(" ", 1)[1].split("-")[1:3]
            repos = self.page(INSTALLED[app][int(inst)])
            return self.reply(200, {"repositories": [{"full_name": r} for r in repos]})
        self.reply(500, {"message": "unexpected " + path})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or "null")
        parts = self.path.split("/")
        if len(parts) == 5 and parts[1:3] == ["app", "installations"] and parts[4] == "access_tokens":
            app = iss(self.headers["Authorization"])
            scope = ",".join(body["repositories"]) if body and "repositories" in body else "all"
            suffix = ""
            permissions = (body or {}).get("permissions")
            if permissions is not None:
                # The model's token must be read-only on the four permissions the spec names.
                if any(v != "read" for v in permissions.values()) or set(permissions) != {
                    "contents", "issues", "metadata", "pull_requests",
                }:
                    return self.reply(400, {"message": "not a read-only token request"})
                suffix = "-ro"
            return self.reply(201, {"token": f"tok-{app}-{parts[3]}-{scope}{suffix}"})
        self.reply(500, {"message": "unexpected " + self.path})

    def log_message(self, *args):
        pass  # silence the per-request log


server = HTTPServer(("127.0.0.1", 0), Handler)
print(server.server_address[1], flush=True)
try:
    server.serve_forever()
except KeyboardInterrupt:
    sys.exit(0)
