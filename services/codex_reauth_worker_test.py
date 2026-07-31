import json
import os
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
