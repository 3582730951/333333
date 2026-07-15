import asyncio
import base64
import importlib.util
import json
import pathlib
import resource
import sys
import threading
import unittest


def load_sidecar():
    path = pathlib.Path(__file__).with_name("curl_cffi_sidecar.py")
    spec = importlib.util.spec_from_file_location("curl_cffi_sidecar", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class AsyncSidecarIntegrationTest(unittest.IsolatedAsyncioTestCase):
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
            body = await reader.read()
            writer.close()
            await writer.wait_closed()
            return body

        tasks = [asyncio.create_task(request()) for _ in range(200)]
        await asyncio.wait_for(all_arrived.wait(), timeout=5)
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
            total = 0
            while chunk := await reader.read(4096):
                total += len(chunk)
                await asyncio.sleep(0.0005)
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


if __name__ == "__main__":
    unittest.main()
