import json
import os
import socket
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request

import codex_reauth_worker as worker


def make_id_token(workspace_id="workspace-ok", user_id="user-ok", email="ok@example.internal"):
    import base64
    payload = json.dumps({
        "https://api.openai.com/profile": {"email": email},
        "https://api.openai.com/auth": {
            "chatgpt_user_id": user_id,
            "chatgpt_account_id": workspace_id,
            "chatgpt_plan_type": "plus",
        },
    }).encode()
    return "header." + base64.urlsafe_b64encode(payload).rstrip(b"=").decode() + ".sig"


class CodexReauthWorkerTests(unittest.TestCase):
    def test_health_endpoint_is_ready_and_does_not_expose_state(self):
        server, url = self.start_server({})
        try:
            with urllib.request.urlopen(url + "/healthz", timeout=5) as resp:
                body = json.loads(resp.read().decode())
                cache_control = resp.headers.get("Cache-Control")
        finally:
            server.shutdown()
            server.server_close()
        self.assertEqual(body, {"ready": True, "service": "codex-reauth-worker"})
        self.assertEqual(cache_control, "no-store")

    def test_extracts_codex_account_and_workspace_from_login_output(self):
        id_token = make_id_token()
        out = "noise\n__CODEX_ACCOUNT__ " + json.dumps({
            "access_token": "at",
            "refresh_token": "rt",
            "id_token": id_token,
            "session_cookie": "cookie=1",
            "email": "ok@example.internal",
        }) + "\n"
        result = worker.parse_login_output(out)
        self.assertEqual(result["access_token"], "at")
        self.assertEqual(worker.workspace_id_from_id_token(result["id_token"]), "workspace-ok")
        normalized = worker.normalize_result(result)
        self.assertEqual(normalized["status"], "succeeded")
        self.assertEqual(normalized["workspace_id"], "workspace-ok")

    def test_workspace_mismatch_is_structured_error(self):
        result = {"access_token": "at", "refresh_token": "rt", "id_token": make_id_token("workspace-wrong")}
        with self.assertRaises(worker.WorkerError) as cm:
            worker.validate_workspace(result, "workspace-ok")
        self.assertEqual(cm.exception.code, "workspace_mismatch")
        self.assertEqual(cm.exception.http_status, 409)

    def test_http_worker_success_uses_configured_login_command(self):
        id_token = make_id_token()
        with tempfile.TemporaryDirectory() as td:
            script = os.path.join(td, "login.py")
            with open(script, "w", encoding="utf-8") as f:
                f.write("import json, os\n")
                f.write("assert os.environ['REG_EMAIL']=='ok@example.internal'\n")
                f.write("assert os.environ['REG_PASSWORD']=='pw'\n")
                f.write("assert os.environ['REG_OTP_URL']=='https://otp.example'\n")
                f.write("assert os.environ['REG_TARGET_WORKSPACE_ID']=='workspace-ok'\n")
                f.write("print('__CODEX_ACCOUNT__ '+json.dumps({'access_token':'at','refresh_token':'rt','id_token':%r,'session_cookie':'cookie=1'}))\n" % id_token)
            server, url = self.start_server({"CODEX_REAUTH_LOGIN_COMMAND": f"python3 {script}"})
            try:
                resp = self.post_json(url + "/v1/codex/reauth", {
                    "email": "ok@example.internal",
                    "password": "pw",
                    "otp_url": "https://otp.example",
                    "target_workspace_id": "workspace-ok",
                })
            finally:
                server.shutdown()
                server.server_close()
            self.assertEqual(resp["status"], "succeeded")
            self.assertEqual(resp["access_token"], "at")
            self.assertEqual(resp["workspace_id"], "workspace-ok")
            self.assertNotIn("password", resp)

    def test_http_worker_maps_otp_timeout(self):
        with tempfile.TemporaryDirectory() as td:
            script = os.path.join(td, "login.py")
            with open(script, "w", encoding="utf-8") as f:
                f.write("import sys\nprint('OTP TIMEOUT — cannot login', file=sys.stderr)\nsys.exit(2)\n")
            server, url = self.start_server({"CODEX_REAUTH_LOGIN_COMMAND": f"python3 {script}"})
            try:
                with self.assertRaises(urllib.error.HTTPError) as cm:
                    self.post_json(url + "/v1/codex/reauth", {"email": "x@example.internal"})
                body = json.loads(cm.exception.read().decode())
            finally:
                server.shutdown()
                server.server_close()
            self.assertEqual(cm.exception.code, 408)
            self.assertEqual(body["status"], "failed")
            self.assertEqual(body["code"], "otp_timeout")

    def test_reauth_concurrency_rejects_while_health_stays_available(self):
        entered = threading.Event()
        release = threading.Event()
        original = worker.handle_reauth

        def blocking_reauth(_payload):
            entered.set()
            release.wait(timeout=5)
            return {"status": "succeeded", "access_token": "at"}

        worker.handle_reauth = blocking_reauth
        server, url = self.start_server({})
        first_result = {}

        def first_request():
            first_result.update(self.post_json(url + "/v1/codex/reauth", {}))

        first = threading.Thread(target=first_request)
        first.start()
        try:
            self.assertTrue(entered.wait(timeout=2))
            with urllib.request.urlopen(url + "/healthz", timeout=2) as response:
                self.assertEqual(json.loads(response.read().decode())["ready"], True)
            with self.assertRaises(urllib.error.HTTPError) as cm:
                self.post_json(url + "/v1/codex/reauth", {})
            body = json.loads(cm.exception.read().decode())
            self.assertEqual(cm.exception.code, 429)
            self.assertEqual(body["code"], "busy")
            deadline = time.time() + 1
            while server.active_workers != 1 and time.time() < deadline:
                time.sleep(0.01)
            self.assertEqual(server.active_workers, 1)
        finally:
            release.set()
            first.join(timeout=5)
            server.shutdown()
            server.server_close()
            worker.handle_reauth = original
        self.assertEqual(first_result["status"], "succeeded")
        self.assertEqual(server.active_workers, 0)

    def test_partial_headers_release_request_slots_and_health_recovers(self):
        server, _ = self.start_server({})
        # Leave ample time for all handler threads to be scheduled before the
        # idle deadline; the recovery assertion below still keeps the test fast.
        server.header_idle_timeout = 2
        connections = []
        try:
            address = ("127.0.0.1", server.server_address[1])
            for _ in range(server.request_thread_limit):
                connection = socket.create_connection(address, timeout=2)
                connection.sendall(b"GET /healthz HTTP/1.1\r\nHost: localhost\r\n")
                connections.append(connection)
            deadline = time.time() + 3
            while server.active_workers != server.request_thread_limit and time.time() < deadline:
                time.sleep(0.01)
            self.assertEqual(server.active_workers, server.request_thread_limit)

            overflow = socket.create_connection(address, timeout=2)
            overflow.sendall(b"GET /healthz HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
            response = overflow.recv(4096)
            overflow.close()
            self.assertIn(b"429 Too Many Requests", response)
            self.assertEqual(server.active_workers, server.request_thread_limit)

            deadline = time.time() + 3
            while server.active_workers and time.time() < deadline:
                time.sleep(0.01)
            self.assertEqual(server.active_workers, 0)
            with urllib.request.urlopen(
                f"http://127.0.0.1:{server.server_address[1]}/healthz",
                timeout=2,
            ) as health:
                self.assertEqual(json.loads(health.read().decode())["ready"], True)
        finally:
            for connection in connections:
                connection.close()
            deadline = time.time() + 2
            while server.active_workers and time.time() < deadline:
                time.sleep(0.01)
            server.shutdown()
            server.server_close()
        self.assertEqual(server.active_workers, 0)

    def test_partial_body_releases_reauth_and_request_slots(self):
        original = worker.handle_reauth
        worker.handle_reauth = lambda _payload: {"status": "succeeded", "access_token": "at"}
        server, url = self.start_server({})
        server.body_idle_timeout = 1
        connection = socket.create_connection(("127.0.0.1", server.server_address[1]), timeout=2)
        try:
            connection.sendall(
                b"POST /v1/codex/reauth HTTP/1.1\r\n"
                b"Host: localhost\r\nContent-Type: application/json\r\n"
                b"Content-Length: 2\r\nConnection: close\r\n\r\n{"
            )
            deadline = time.time() + 1
            while server.active_workers != 1 and time.time() < deadline:
                time.sleep(0.01)
            self.assertEqual(server.active_workers, 1)

            with self.assertRaises(urllib.error.HTTPError) as cm:
                self.post_json(url + "/v1/codex/reauth", {})
            self.assertEqual(cm.exception.code, 429)

            deadline = time.time() + 2
            while server.active_workers and time.time() < deadline:
                time.sleep(0.01)
            self.assertEqual(server.active_workers, 0)
            self.assertEqual(
                self.post_json(url + "/v1/codex/reauth", {})["status"],
                "succeeded",
            )
            with urllib.request.urlopen(url + "/healthz", timeout=2) as health:
                self.assertEqual(json.loads(health.read().decode())["ready"], True)
        finally:
            connection.close()
            deadline = time.time() + 2
            while server.active_workers and time.time() < deadline:
                time.sleep(0.01)
            server.shutdown()
            server.server_close()
            worker.handle_reauth = original
        self.assertEqual(server.active_workers, 0)

    def start_server(self, extra_env):
        old = os.environ.copy()
        os.environ.update(extra_env)
        httpd = worker.make_server("127.0.0.1", 0, concurrency=1)
        os.environ.clear(); os.environ.update(old); os.environ.update(extra_env)
        thread = threading.Thread(target=httpd.serve_forever, daemon=True)
        thread.start()
        time.sleep(0.05)
        return httpd, f"http://127.0.0.1:{httpd.server_address[1]}"

    def post_json(self, url, payload):
        req = urllib.request.Request(url, data=json.dumps(payload).encode(), headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=5) as resp:
            return json.loads(resp.read().decode())


if __name__ == "__main__":
    unittest.main()
