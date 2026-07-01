"""Local HTTP proxy: app -> v2rayN (1st hop) -> Cliproxy (2nd hop) -> target."""
from __future__ import annotations

import logging
import select
import socket
import socketserver
import threading
from typing import Optional
from urllib.parse import urlparse

import socks

log = logging.getLogger(__name__)

_manager_lock = threading.Lock()
_manager: Optional["ChainProxyManager"] = None


def _parse_proxy_url(url: str) -> tuple[int, str, int]:
    u = urlparse(url)
    scheme = (u.scheme or "http").lower()
    host = u.hostname or "127.0.0.1"
    port = u.port or (10808 if scheme.startswith("socks") else 10809)
    if scheme in ("socks5", "socks5h"):
        return socks.SOCKS5, host, port
    if scheme in ("http", "https"):
        return socks.HTTP, host, port
    raise ValueError(f"unsupported upstream proxy scheme: {scheme}")


def _parse_host_port(raw: str) -> tuple[str, int]:
    raw = raw.replace("http://", "").replace("https://", "")
    if "@" in raw:
        raw = raw.rsplit("@", 1)[-1]
    host, port = raw.rsplit(":", 1)
    return host, int(port)


def _connect_via(upstream: tuple[int, str, int], host: str, port: int) -> socket.socket:
    sock = socks.socksocket()
    kind, up_host, up_port = upstream
    sock.set_proxy(kind, up_host, up_port)
    sock.settimeout(30)
    sock.connect((host, port))
    return sock


def _relay(a: socket.socket, b: socket.socket) -> None:
    sockets = [a, b]
    while True:
        readable, _, errored = select.select(sockets, [], sockets, 120)
        if errored or not readable:
            break
        for src in readable:
            dst = b if src is a else a
            try:
                data = src.recv(65536)
            except OSError:
                return
            if not data:
                return
            try:
                dst.sendall(data)
            except OSError:
                return


class _ChainHandler(socketserver.StreamRequestHandler):
    upstream: tuple[int, str, int]
    clip_host: str
    clip_port: int

    def handle(self) -> None:
        try:
            first = self.rfile.readline().decode("latin-1", errors="replace").strip()
            if not first:
                return
            parts = first.split(" ")
            if len(parts) < 2:
                return
            method, target = parts[0], parts[1]

            headers = []
            while True:
                line = self.rfile.readline()
                if not line or line in (b"\r\n", b"\n"):
                    break
                headers.append(line)

            remote = _connect_via(self.upstream, self.clip_host, self.clip_port)
            try:
                if method.upper() == "CONNECT":
                    host, port_s = target.split(":")
                    port = int(port_s)
                    req = (
                        f"CONNECT {host}:{port} HTTP/1.1\r\n"
                        f"Host: {host}:{port}\r\n"
                        f"Proxy-Connection: Keep-Alive\r\n\r\n"
                    ).encode()
                    remote.sendall(req)
                    resp = b""
                    while b"\r\n\r\n" not in resp and len(resp) < 8192:
                        chunk = remote.recv(4096)
                        if not chunk:
                            break
                        resp += chunk
                    status_line = resp.split(b"\r\n", 1)[0]
                    parts = status_line.split()
                    if len(parts) < 2 or parts[1] != b"200":
                        log.debug(
                            "chain CONNECT via %s:%s rejected: %r",
                            self.clip_host,
                            self.clip_port,
                            status_line[:120],
                        )
                        self.connection.sendall(resp or b"HTTP/1.1 502 Bad Gateway\r\n\r\n")
                        return
                    self.connection.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
                    _relay(self.connection, remote)
                    return

                # Plain HTTP proxy request
                if target.startswith("http://") or target.startswith("https://"):
                    u = urlparse(target)
                    path = u.path or "/"
                    if u.query:
                        path = f"{path}?{u.query}"
                    req = f"{method} {path} HTTP/1.1\r\n".encode()
                    req += f"Host: {u.hostname}\r\n".encode()
                    req += b"".join(headers)
                    req += b"\r\n"
                    remote.sendall(req)
                    _relay(self.connection, remote)
            finally:
                remote.close()
        except Exception as exc:
            log.debug("chain proxy handler error: %s", exc)


class _ThreadingServer(socketserver.ThreadingMixIn, socketserver.TCPServer):
    allow_reuse_address = True
    daemon_threads = True


class ChainProxyServer:
    def __init__(self, cliproxy: str, upstream: str, listen_host: str = "127.0.0.1", listen_port: int = 0):
        self.cliproxy = cliproxy
        self.upstream = upstream
        self.listen_host = listen_host
        self.listen_port = listen_port
        self._clip_host, self._clip_port = _parse_host_port(cliproxy)
        self._upstream = _parse_proxy_url(upstream)
        self._server: Optional[_ThreadingServer] = None
        self._thread: Optional[threading.Thread] = None

    @property
    def url(self) -> str:
        if not self._server:
            raise RuntimeError("chain proxy not started")
        host, port = self._server.server_address[:2]
        return f"http://{host}:{port}"

    def start(self) -> str:
        if self._server:
            return self.url

        handler_cls = type(
            "BoundChainHandler",
            (_ChainHandler,),
            {
                "upstream": self._upstream,
                "clip_host": self._clip_host,
                "clip_port": self._clip_port,
            },
        )
        self._server = _ThreadingServer((self.listen_host, self.listen_port), handler_cls)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True, name="chain-proxy")
        self._thread.start()
        log.info(
            "Chain proxy listening on %s via %s -> %s:%s",
            self.url,
            self.upstream,
            self._clip_host,
            self._clip_port,
        )
        return self.url

    def stop(self) -> None:
        if self._server:
            self._server.shutdown()
            self._server.server_close()
            self._server = None


class ChainProxyManager:
    def __init__(self, upstream: str):
        self.upstream = upstream
        self._server: Optional[ChainProxyServer] = None
        self._cliproxy = ""

    def get(self, cliproxy: str) -> str:
        if self._server and self._cliproxy == cliproxy:
            return self._server.url
        if self._server:
            self._server.stop()
        self._cliproxy = cliproxy
        self._server = ChainProxyServer(cliproxy, self.upstream)
        return self._server.start()


def get_chain_proxy_url(cliproxy: str, upstream: str) -> str:
    global _manager
    with _manager_lock:
        if _manager is None or _manager.upstream != upstream:
            if _manager and _manager._server:
                _manager._server.stop()
            _manager = ChainProxyManager(upstream)
        return _manager.get(cliproxy)


def reset_chain_proxy_manager() -> None:
    """Drop cached chain listener (e.g. after probe failure or expired Cliproxy IP)."""
    global _manager
    with _manager_lock:
        if _manager and _manager._server:
            self_url = _manager._server.url
            log.info("Stopping chain proxy %s", self_url)
            _manager._server.stop()
        _manager = None
