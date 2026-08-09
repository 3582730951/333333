import asyncio
import base64
import importlib.util
import json
import pathlib
import resource
import sys
import tempfile
import threading
import unittest


def load_sidecar():
    path = pathlib.Path(__file__).with_name("curl_cffi_sidecar.py")
    spec = importlib.util.spec_from_file_location("curl_cffi_sidecar", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class NativeHeaderShapeTest(unittest.TestCase):
    def test_claude_connection_and_accept_encoding_overrides(self):
        sidecar = load_sidecar()
        headers = {
            "Accept": ["application/json"],
            "anthropic-version": ["2023-06-01"],
            "Connection": ["keep-alive"],
            "Host": ["wrong.test"],
            "Accept-Encoding": ["identity"],
            "Content-Length": ["9"],
        }
        cleaned = sidecar.clean_headers(headers, preserve_connection=True)
        self.assertEqual(list(cleaned), ["Accept", "anthropic-version", "Connection"])
        self.assertEqual(cleaned["Connection"], "keep-alive")
        self.assertNotIn("Connection", sidecar.clean_headers(headers))
        self.assertEqual(
            sidecar.request_accept_encoding({"accept_encoding": "gzip, deflate, br, zstd"}),
            "gzip, deflate, br, zstd",
        )
        self.assertEqual(
            sidecar.request_accept_encoding({"accept_encoding": "gzip\r\nx-relay: 1"}),
            sidecar.ACCEPT_ENCODING,
        )
        algorithms = ["ecdsa_secp256r1_sha256", "rsa_pkcs1_sha1"]
        self.assertEqual(
            sidecar.raw_ja3_extra_fp({"tls_signature_algorithms": algorithms}),
            {
                "tls_grease": False,
                "tls_permute_extensions": False,
                "tls_signature_algorithms": algorithms,
            },
        )
        self.assertNotIn(
            "tls_signature_algorithms",
            sidecar.raw_ja3_extra_fp({"tls_signature_algorithms": ["bad,value"]}),
        )
        ordered = sidecar.request_headers(
            {
                "headers": headers,
                "preserve_connection_header": True,
                "native_header_order": True,
                "header_order": [
                    "accept", "anthropic-version", "connection", "host",
                    "accept-encoding", "content-length",
                ],
            },
            "https://api.anthropic.com/v1/messages",
            "POST",
            "gzip, deflate, br, zstd",
            9,
        )
        self.assertEqual(
            list(ordered),
            ["Accept", "anthropic-version", "Connection", "Host", "Accept-Encoding", "Content-Length"],
        )
        self.assertEqual(ordered["Host"], "api.anthropic.com")
        self.assertEqual(ordered["Content-Length"], "9")

    def test_raw_ja3_session_does_not_inherit_chrome_impersonation(self):
        sidecar = load_sidecar()
        created = []

        class FakeSession:
            def __init__(self, **kwargs):
                created.append(kwargs)

            async def close(self):
                pass

        original = sidecar.AsyncSession
        sidecar.AsyncSession = FakeSession
        try:
            asyncio.run(sidecar.session_for(("", "771,1,0,29,0", "", "raw-ja3", "v1")))
            asyncio.run(sidecar.release_session(("", "771,1,0,29,0", "", "raw-ja3", "v1")))
            asyncio.run(sidecar.session_for(("", "", "", "browser", "v2")))
            asyncio.run(sidecar.release_session(("", "", "", "browser", "v2")))
        finally:
            sidecar.AsyncSession = original
            sidecar._sessions.clear()
            sidecar._session_active.clear()
        self.assertIsNone(created[0]["impersonate"])
        self.assertEqual(created[1]["impersonate"], sidecar.IMPERSONATE)


async def read_chunked_body(reader):
    """Decode the sidecar v2 HTTP/1.1 chunked relay body and trailers."""
    body = bytearray()
    trailers = {}
    while True:
        line = await reader.readuntil(b"\r\n")
        size = int(line.split(b";", 1)[0].strip(), 16)
        if size == 0:
            while True:
                line = await reader.readuntil(b"\r\n")
                if line == b"\r\n":
                    return bytes(body), trailers
                key, value = line.decode("latin1").split(":", 1)
                trailers[key.lower()] = value.strip()
        body.extend(await reader.readexactly(size))
        await reader.readexactly(2)


class AsyncSidecarIntegrationTest(unittest.IsolatedAsyncioTestCase):
    async def test_header_idle_timeout_returns_408_without_using_inflight(self):
        sidecar = load_sidecar()
        sidecar.HEADER_IDLE_TIMEOUT = 0.05
        sidecar_server = await asyncio.start_server(sidecar.client, "127.0.0.1", 0)
        sidecar_port = sidecar_server.sockets[0].getsockname()[1]

        reader, writer = await asyncio.open_connection("127.0.0.1", sidecar_port)
        writer.write(b"POST /proxy HTTP/1.1\r\nhost: local\r\n")
        await writer.drain()
        head = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), timeout=2)
        body = await reader.read()

        self.assertIn(b"408 Request Timeout", head)
        self.assertIn(b"x-sidecar-error-code: sidecar_header_idle_timeout", head.lower())
        self.assertEqual(json.loads(body)["error"]["phase"], "preflight")
        self.assertEqual(sidecar._inflight, 0)

        writer.close()
        await writer.wait_closed()
        sidecar_server.close()
        await sidecar_server.wait_closed()

    async def test_body_idle_timeout_releases_single_inflight_slot(self):
        sidecar = load_sidecar()
        sidecar.MAX_INFLIGHT = 1
        sidecar.BODY_IDLE_TIMEOUT = 0.5
        admitted = []

        async def fake_proxy(writer, payload, body):
            body.rewind()
            admitted.append((payload, body.file.read()))
            await sidecar.send(writer, 200, {"content-type": "text/plain"}, b"ok")

        sidecar.proxy = fake_proxy
        with tempfile.TemporaryDirectory() as spool_dir:
            sidecar.SPOOL_DIR = spool_dir
            sidecar.SPOOL_RESERVE_BYTES = 0
            sidecar_server = await asyncio.start_server(sidecar.client, "127.0.0.1", 0)
            sidecar_port = sidecar_server.sockets[0].getsockname()[1]
            meta = base64.b64encode(json.dumps({"method": "POST", "url": "http://local/"}).encode()).decode()

            slow_reader, slow_writer = await asyncio.open_connection("127.0.0.1", sidecar_port)
            slow_writer.write(f"POST /proxy HTTP/1.1\r\nx-sidecar-meta: {meta}\r\ncontent-length: 4\r\n\r\nx".encode())
            await slow_writer.drain()
            for _ in range(200):
                if sidecar._inflight == 1 and sidecar._spool_bytes == 4:
                    break
                await asyncio.sleep(0.005)
            self.assertEqual(sidecar._inflight, 1)
            self.assertEqual(sidecar._spool_bytes, 4)

            timeout_head = await asyncio.wait_for(slow_reader.readuntil(b"\r\n\r\n"), timeout=2)
            timeout_body = await slow_reader.read()
            self.assertIn(b"408 Request Timeout", timeout_head)
            self.assertIn(b"x-sidecar-error-code: sidecar_body_idle_timeout", timeout_head.lower())
            self.assertEqual(json.loads(timeout_body)["error"]["phase"], "preflight")
            self.assertEqual(sidecar._inflight, 0)
            self.assertEqual(sidecar._spool_bytes, 0)
            self.assertEqual(list(pathlib.Path(spool_dir).iterdir()), [])

            good_reader, good_writer = await asyncio.open_connection("127.0.0.1", sidecar_port)
            good_writer.write(f"POST /proxy HTTP/1.1\r\nx-sidecar-meta: {meta}\r\ncontent-length: 2\r\n\r\nok".encode())
            await good_writer.drain()
            good_head = await asyncio.wait_for(good_reader.readuntil(b"\r\n\r\n"), timeout=2)
            good_body = await good_reader.read()
            self.assertIn(b"200 OK", good_head)
            self.assertEqual(good_body, b"ok")
            self.assertEqual(len(admitted), 1)
            self.assertEqual(admitted[0][1], b"ok")
            self.assertEqual(sidecar._inflight, 0)
            self.assertEqual(sidecar._spool_bytes, 0)

            slow_writer.close(); good_writer.close()
            await slow_writer.wait_closed(); await good_writer.wait_closed()
            sidecar_server.close()
            await sidecar_server.wait_closed()

    async def test_inflight_capacity_rejects_before_spooling_body(self):
        sidecar = load_sidecar()
        sidecar.IMPERSONATE = "chrome"
        sidecar.MAX_INFLIGHT = 1
        held = asyncio.Event()
        release = asyncio.Event()

        async def upstream(reader, writer):
            await reader.readuntil(b"\r\n\r\n")
            held.set()
            await release.wait()
            writer.write(b"HTTP/1.1 200 OK\r\ncontent-length: 2\r\nconnection: close\r\n\r\nok")
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        with tempfile.TemporaryDirectory() as spool_dir:
            sidecar.SPOOL_DIR = spool_dir
            sidecar.SPOOL_RESERVE_BYTES = 0
            upstream_server = await asyncio.start_server(upstream, "127.0.0.1", 0)
            upstream_port = upstream_server.sockets[0].getsockname()[1]
            sidecar_server = await asyncio.start_server(sidecar.client, "127.0.0.1", 0)
            sidecar_port = sidecar_server.sockets[0].getsockname()[1]
            meta = base64.b64encode(json.dumps({"method": "GET", "url": f"http://127.0.0.1:{upstream_port}/", "cookie_jar_key": "capacity"}).encode()).decode()

            first_reader, first_writer = await asyncio.open_connection("127.0.0.1", sidecar_port)
            first_writer.write(f"POST /proxy HTTP/1.1\r\nx-sidecar-meta: {meta}\r\ncontent-length: 0\r\n\r\n".encode())
            await first_writer.drain()
            await asyncio.wait_for(held.wait(), timeout=10)

            metrics_reader, metrics_writer = await asyncio.open_connection("127.0.0.1", sidecar_port)
            metrics_writer.write(b"GET /metrics HTTP/1.1\r\ncontent-length: 0\r\n\r\n")
            await metrics_writer.drain()
            metrics_head = await metrics_reader.readuntil(b"\r\n\r\n")
            metrics_body = await metrics_reader.read()
            self.assertIn(b"200 OK", metrics_head)
            self.assertEqual(json.loads(metrics_body)["inflight"], 1)
            metrics_writer.close()
            await metrics_writer.wait_closed()

            second_reader, second_writer = await asyncio.open_connection("127.0.0.1", sidecar_port)
            # Do not send these claimed bytes: a capacity rejection must arrive
            # after headers, before the body reader/spool is entered.
            second_writer.write(f"POST /proxy HTTP/1.1\r\nx-sidecar-meta: {meta}\r\ncontent-length: {8 << 20}\r\n\r\n".encode())
            await second_writer.drain()
            rejected_head = await asyncio.wait_for(second_reader.readuntil(b"\r\n\r\n"), timeout=2)
            rejected_body = await second_reader.read()
            self.assertIn(b"503 Service Unavailable", rejected_head)
            self.assertIn(b"retry-after: 1", rejected_head.lower())
            self.assertIn(b"x-sidecar-error-code: sidecar_capacity_exhausted", rejected_head.lower())
            self.assertEqual(json.loads(rejected_body)["error"]["phase"], "preflight")
            self.assertEqual(sidecar.metrics()["rejected_capacity"], 1)
            self.assertEqual(sidecar.metrics()["limit"], 1)
            self.assertEqual(list(pathlib.Path(spool_dir).iterdir()), [])

            release.set()
            await first_reader.readuntil(b"\r\n\r\n")
            await first_reader.read()
            first_writer.close(); second_writer.close()
            await first_writer.wait_closed(); await second_writer.wait_closed()
            sidecar_server.close(); upstream_server.close()
            await sidecar_server.wait_closed(); await upstream_server.wait_closed()
            for session, _ in list(sidecar._sessions.values()):
                await session.close()

    async def test_session_pool_never_exceeds_active_limit_and_evicts_lru_idle(self):
        sidecar = load_sidecar()
        sidecar.IMPERSONATE = "chrome"
        sidecar.MAX_BUCKETS = 2
        one = ("", "", "", "one")
        two = ("", "", "", "two")
        three = ("", "", "", "three")
        await sidecar.session_for(one)
        await sidecar.session_for(two)
        with self.assertRaises(sidecar.SidecarCapacityError) as raised:
            await sidecar.session_for(three)
        self.assertEqual(raised.exception.code, "sidecar_session_capacity")
        self.assertEqual(len(sidecar._sessions), 2)
        self.assertEqual(set(sidecar._sessions), {one, two})
        self.assertNotIn(three, sidecar._session_active)

        await sidecar.release_session(one)
        await sidecar.release_session(two)
        await sidecar.session_for(three)
        self.assertEqual(len(sidecar._sessions), 2)
        self.assertNotIn(one, sidecar._sessions)
        self.assertIn(two, sidecar._sessions)
        self.assertIn(three, sidecar._sessions)
        await sidecar.release_session(three)
        for session, _ in list(sidecar._sessions.values()):
            await session.close()

    async def test_v2_request_body_spools_and_uploads_from_file(self):
        sidecar = load_sidecar()
        sidecar.IMPERSONATE = "chrome"
        payload = b"large-context-" * (192 << 10)
        arrived = asyncio.Event()
        release = asyncio.Event()
        received = {}

        with tempfile.TemporaryDirectory() as spool_dir:
            sidecar.SPOOL_DIR = spool_dir
            sidecar.SPOOL_RESERVE_BYTES = 0
            sidecar.SPOOL_MAX_BYTES = 16 << 20
            sidecar.MAX_BODY_BYTES = 16 << 20

            async def upstream(reader, writer):
                head = await reader.readuntil(b"\r\n\r\n")
                headers = {}
                for line in head.decode("latin1").split("\r\n")[1:]:
                    if ":" in line:
                        key, value = line.split(":", 1)
                        headers[key.lower()] = value.strip()
                size = int(headers["content-length"])
                received["body"] = await reader.readexactly(size)
                arrived.set()
                await release.wait()
                writer.write(b"HTTP/1.1 200 OK\r\ncontent-length: 2\r\nconnection: close\r\n\r\nok")
                await writer.drain()
                writer.close()
                await writer.wait_closed()

            upstream_server = await asyncio.start_server(upstream, "127.0.0.1", 0)
            upstream_port = upstream_server.sockets[0].getsockname()[1]
            sidecar_server = await asyncio.start_server(sidecar.client, "127.0.0.1", 0)
            sidecar_port = sidecar_server.sockets[0].getsockname()[1]
            meta = base64.b64encode(json.dumps({
                "method": "POST",
                "url": f"http://127.0.0.1:{upstream_port}/upload",
                "headers": {"content-type": ["application/json"]},
                "cookie_jar_key": "spooled-upload",
            }).encode()).decode()

            async def request():
                reader, writer = await asyncio.open_connection("127.0.0.1", sidecar_port)
                writer.write(f"POST /proxy HTTP/1.1\r\nx-sidecar-meta: {meta}\r\ncontent-length: {len(payload)}\r\n\r\n".encode())
                writer.write(payload)
                await writer.drain()
                head = await reader.readuntil(b"\r\n\r\n")
                self.assertIn(b"x-sidecar-upstream-status: 200", head.lower())
                body, trailers = await read_chunked_body(reader)
                writer.close()
                await writer.wait_closed()
                return body, trailers

            task = asyncio.create_task(request())
            await asyncio.wait_for(arrived.wait(), timeout=10)
            self.assertEqual(sidecar._spool_bytes, len(payload))
            self.assertEqual(len(list(pathlib.Path(spool_dir).iterdir())), 1)
            release.set()
            body, trailers = await task
            self.assertEqual(body, b"ok")
            self.assertEqual(trailers, {})
            self.assertEqual(received["body"], payload)
            self.assertEqual(sidecar._spool_bytes, 0)
            self.assertEqual(list(pathlib.Path(spool_dir).iterdir()), [])

            sidecar_server.close()
            upstream_server.close()
            await sidecar_server.wait_closed()
            await upstream_server.wait_closed()

    async def test_two_hundred_streams_reach_upstream(self):
        sidecar = load_sidecar()
        sidecar.IMPERSONATE = "chrome"
        arrived = 0
        all_arrived = asyncio.Event()
        release = asyncio.Event()

        async def upstream(reader, writer):
            nonlocal arrived
            await reader.readuntil(b"\r\n\r\n")
            arrived += 1
            if arrived == 200:
                all_arrived.set()
            writer.write(b"HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\nconnection: close\r\n\r\ndata: first\n\n")
            await writer.drain()
            await release.wait()
            writer.write(b"data: done\n\n")
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        upstream_server = await asyncio.start_server(upstream, "127.0.0.1", 0, backlog=1024)
        upstream_port = upstream_server.sockets[0].getsockname()[1]
        sidecar_server = await asyncio.start_server(sidecar.client, "127.0.0.1", 0, backlog=1024)
        sidecar_port = sidecar_server.sockets[0].getsockname()[1]

        meta = base64.b64encode(json.dumps({
            "method": "GET",
            "url": f"http://127.0.0.1:{upstream_port}/stream",
            "headers": {},
            "cookie_jar_key": "account-a",
        }).encode()).decode()

        async def request():
            reader, writer = await asyncio.open_connection("127.0.0.1", sidecar_port)
            raw = f"POST /proxy HTTP/1.1\r\nhost: local\r\nx-sidecar-meta: {meta}\r\ncontent-length: 0\r\n\r\n".encode()
            writer.write(raw)
            await writer.drain()
            head = await reader.readuntil(b"\r\n\r\n")
            self.assertIn(b"x-sidecar-upstream-status: 200", head.lower())
            body, trailers = await read_chunked_body(reader)
            self.assertEqual(trailers, {})
            writer.close()
            await writer.wait_closed()
            return body

        tasks = [asyncio.create_task(request()) for _ in range(200)]
        # curl_cffi creates 200 browser-profile connections in one event loop;
        # constrained CI runners can spend several seconds in that native setup
        # before every upstream socket is visible. This guards capacity, not a
        # five-second latency SLO, so keep the deadline generous enough to avoid
        # conflating scheduler contention with a lost stream.
        await asyncio.wait_for(all_arrived.wait(), timeout=10)
        self.assertEqual(sidecar._inflight, 200)
        release.set()
        bodies = await asyncio.gather(*tasks)
        self.assertTrue(all(b"data: first" in body and b"data: done" in body for body in bodies))
        self.assertEqual(sidecar._inflight, 0)

        sidecar_server.close()
        upstream_server.close()
        await sidecar_server.wait_closed()
        await upstream_server.wait_closed()
        for session, _ in list(sidecar._sessions.values()):
            await session.close()

    async def test_slow_consumers_keep_threads_and_rss_bounded(self):
        sidecar = load_sidecar()
        sidecar.IMPERSONATE = "chrome"
        payload = b"x" * (2 << 20)

        async def upstream(reader, writer):
            await reader.readuntil(b"\r\n\r\n")
            writer.write(b"HTTP/1.1 200 OK\r\ncontent-type: application/octet-stream\r\nconnection: close\r\n\r\n")
            for offset in range(0, len(payload), 16 << 10):
                writer.write(payload[offset:offset + (16 << 10)])
                await writer.drain()
            writer.close()
            await writer.wait_closed()

        upstream_server = await asyncio.start_server(upstream, "127.0.0.1", 0, backlog=128)
        upstream_port = upstream_server.sockets[0].getsockname()[1]
        sidecar_server = await asyncio.start_server(sidecar.client, "127.0.0.1", 0, backlog=128)
        sidecar_port = sidecar_server.sockets[0].getsockname()[1]
        meta = base64.b64encode(json.dumps({"method": "GET", "url": f"http://127.0.0.1:{upstream_port}/", "cookie_jar_key": "slow"}).encode()).decode()
        threads_before = threading.active_count()
        rss_before = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss

        async def slow_request():
            reader, writer = await asyncio.open_connection("127.0.0.1", sidecar_port)
            writer.write(f"POST /proxy HTTP/1.1\r\nx-sidecar-meta: {meta}\r\ncontent-length: 0\r\n\r\n".encode())
            await writer.drain()
            await reader.readuntil(b"\r\n\r\n")
            body, trailers = await read_chunked_body(reader)
            self.assertEqual(trailers, {})
            total = len(body)
            writer.close()
            await writer.wait_closed()
            return total

        totals = await asyncio.gather(*(slow_request() for _ in range(20)))
        rss_after = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
        scale = 1024 if sys.platform != "darwin" else 1
        self.assertTrue(all(total == len(payload) for total in totals))
        self.assertLessEqual(threading.active_count(), threads_before + 4)
        self.assertLessEqual((rss_after - rss_before) * scale, 64 << 20)

        sidecar_server.close(); upstream_server.close()
        await sidecar_server.wait_closed(); await upstream_server.wait_closed()
        for session, _ in list(sidecar._sessions.values()): await session.close()


    async def test_default_headers_flag_suppresses_browser_fingerprint(self):
        # End-to-end guard for the relay-fingerprint fix: {"default_headers": false} in the
        # meta must stop curl-impersonate from injecting the Chrome browser header set
        # (sec-ch-ua*, sec-fetch-*, accept-language, upgrade-insecure-requests) on top of the
        # caller's authentic client headers. Absent flag = historical behavior (injected).
        sidecar = load_sidecar()
        sidecar.IMPERSONATE = "chrome"
        captured = {}

        async def upstream(reader, writer):
            head = await reader.readuntil(b"\r\n\r\n")
            names = set()
            for line in head.decode("latin1").split("\r\n")[1:]:
                if ":" in line:
                    names.add(line.split(":", 1)[0].strip().lower())
            captured["names"] = names
            writer.write(b"HTTP/1.1 200 OK\r\ncontent-type: text/plain\r\nconnection: close\r\n\r\nok")
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        upstream_server = await asyncio.start_server(upstream, "127.0.0.1", 0, backlog=16)
        upstream_port = upstream_server.sockets[0].getsockname()[1]
        sidecar_server = await asyncio.start_server(sidecar.client, "127.0.0.1", 0, backlog=16)
        sidecar_port = sidecar_server.sockets[0].getsockname()[1]

        # An authentic non-browser client header set (mirrors the Go applyClaudeHeaders shape).
        client_headers = {
            "User-Agent": "claude-cli/2.1.226 (external, cli)",
            "X-Stainless-Lang": "js",
            "Anthropic-Version": "2023-06-01",
            "Accept": "application/json",
        }
        browser_only = {"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
                        "sec-fetch-site", "sec-fetch-mode", "sec-fetch-user", "sec-fetch-dest",
                        "upgrade-insecure-requests", "accept-language"}

        async def call(default_headers):
            meta_obj = {
                "method": "POST",
                "url": f"http://127.0.0.1:{upstream_port}/v1/messages",
                "headers": {k: [v] for k, v in client_headers.items()},
                "cookie_jar_key": f"dh-{default_headers}",
            }
            if default_headers is not None:
                meta_obj["default_headers"] = default_headers
            meta = base64.b64encode(json.dumps(meta_obj).encode()).decode()
            reader, writer = await asyncio.open_connection("127.0.0.1", sidecar_port)
            writer.write(f"POST /proxy HTTP/1.1\r\nhost: local\r\nx-sidecar-meta: {meta}\r\ncontent-length: 0\r\n\r\n".encode())
            await writer.drain()
            await reader.readuntil(b"\r\n\r\n")
            await reader.read()
            writer.close()
            await writer.wait_closed()
            return captured["names"]

        # Suppressed: the Claude path. No browser-only header may reach the upstream, and the
        # authentic client headers must survive.
        suppressed = await call(False)
        leaked = suppressed & browser_only
        self.assertEqual(leaked, set(), f"browser headers leaked despite default_headers=false: {leaked}")
        self.assertIn("user-agent", suppressed)
        self.assertIn("x-stainless-lang", suppressed)

        # Absent flag: historical browser-shaped behavior (Codex OAuth / registration). This
        # asserts the flag is what does the suppression — the injection really happens otherwise.
        injected = await call(None)
        self.assertIn("sec-ch-ua", injected, "impersonation did not inject browser headers by default")

        sidecar_server.close(); upstream_server.close()
        await sidecar_server.wait_closed(); await upstream_server.wait_closed()
        for session, _ in list(sidecar._sessions.values()): await session.close()


if __name__ == "__main__":
    unittest.main()
