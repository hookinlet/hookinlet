#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Pat Ramsey
"""A stand-in for the service the relay forwards to: it appends one line per POST
(the X-Hookinlet-Seq header, then the body) to a file and answers 200.

    python3 echo-receiver.py 9099 received.log
"""
import http.server
import sys


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        with open(sys.argv[2], "ab") as f:
            f.write(("seq=%s " % self.headers.get("X-Hookinlet-Seq")).encode() + body + b"\n")
        self.send_response(200)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def log_message(self, *args):  # quiet
        pass


if __name__ == "__main__":
    http.server.ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
