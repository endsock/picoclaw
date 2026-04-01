#!/usr/bin/env python3
import json
import sys
from datetime import datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pymysql

HOST = "0.0.0.0"
PORT = 9000

DB_HOST = "mysql-pops01cn-rw.svc.rezen.work"
DB_USER = "picobot_w"
DB_PASSWORD = "4bqypF5srLc8U38f"
DB_NAME = "picobot"


def update_mysql_slow_sql(md5, result):
    conn = pymysql.connect(
        host=DB_HOST,
        user=DB_USER,
        password=DB_PASSWORD,
        database=DB_NAME,
        charset="utf8mb4",
        autocommit=True,
    )
    try:
        with conn.cursor() as cursor:
            affected = cursor.execute(
                "update mysql_slow_sql set `status`=%s, assistant_ai=%s where md5=%s",
                (2, result, md5),
            )
            print(f"MySQL updated: md5={md5}, affected_rows={affected}")
    finally:
        conn.close()


class WebhookHandler(BaseHTTPRequestHandler):
    server_version = "SimpleWebhook/1.0"

    def log_message(self, format, *args):
        return

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw_body = self.rfile.read(length) if length > 0 else b""
        body_text = raw_body.decode("utf-8", errors="replace")
        payload = None

        now = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
        print("=" * 80)
        print(f"[{now}] Webhook received")
        print(f"Method: {self.command}")
        print(f"Path:   {self.path}")
        print("Headers:")
        for key, value in self.headers.items():
            print(f"  {key}: {value}")

        print("Body:")
        if body_text:
            try:
                payload = json.loads(body_text)
                print(json.dumps(payload, ensure_ascii=False, indent=2))
            except json.JSONDecodeError:
                print(body_text)
        else:
            print("  <empty>")

        if isinstance(payload, dict):
            metadata = payload.get("metadata") or {}
            if (
                payload.get("status") == "completed"
                and metadata.get("task_type") == "mysql_slow_sql"
            ):
                try:
                    update_mysql_slow_sql(metadata.get("md5"), payload.get("result"))
                except Exception as e:
                    print(f"MySQL update failed: {e}")

        print("=" * 80, flush=True)

        response = {
            "ok": True,
            "message": "webhook received",
            "path": self.path,
        }
        data = json.dumps(response, ensure_ascii=False).encode("utf-8")

        self.send_response(200)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        data = json.dumps({
            "ok": True,
            "message": "webhook server is running",
            "post_to": "/callback",
        }, ensure_ascii=False).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


if __name__ == "__main__":
    if len(sys.argv) > 1:
        PORT = int(sys.argv[1])

    server = ThreadingHTTPServer((HOST, PORT), WebhookHandler)
    print(f"Webhook server listening on http://127.0.0.1:{PORT}")
    print("Send webhook requests to e.g. http://127.0.0.1:%d/callback" % PORT)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\nShutting down...")
    finally:
        server.server_close()
