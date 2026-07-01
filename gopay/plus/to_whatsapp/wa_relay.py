"""WhatsApp Web sidecar (Node) lifecycle + state reader.

Single-instance daemon manager: at most one Node process runs.
"""
import json
import os
import shutil
import signal
import subprocess
import threading
import time
from pathlib import Path
from typing import Optional

_lock = threading.Lock()
_proc: Optional[subprocess.Popen] = None
_mode: str = ""        # "pairing" | ""
_started_at: Optional[float] = None

# Base directory for the relay component
BASE_DIR = Path(__file__).resolve().parent

def _data_dir() -> Path:
    d = BASE_DIR / "data"
    d.mkdir(parents=True, exist_ok=True)
    return d

def _state_path() -> Path:
    return _session_dir() / "wa_state.json"

def _session_dir() -> Path:
    p = _data_dir() / "wa_session"
    p.mkdir(parents=True, exist_ok=True)
    return p

def is_running() -> bool:
    global _proc
    return _proc is not None and _proc.poll() is None

def status() -> dict:
    """Read state file written by the Node sidecar."""
    sp = _state_path()
    running = is_running()
    base = {
        "running": running,
        "pid": (_proc.pid if running and _proc else None),
        "mode": _mode,
        "started_at": _started_at,
    }
    if sp.exists():
        try:
            base.update(json.loads(sp.read_text(encoding="utf-8")))
        except Exception as e:
            base["state_read_error"] = str(e)

    if not running:
        base["status"] = "stopped"
        base.pop("code", None)
    elif "status" not in base:
        base["status"] = "starting"

    return base

def start(mode: str = "pairing", pairing_phone: str = "") -> dict:
    """Spawn the Node sidecar."""
    global _proc, _mode, _started_at

    mode = (mode or "pairing").lower()
    if mode != "pairing":
        raise ValueError(f"whatsapp-relay only supports pairing mode, got {mode!r}")

    digits = "".join(ch for ch in (pairing_phone or "") if ch.isdigit())
    if len(digits) < 10:
        raise ValueError("Pairing mode requires pairing_phone (incl. country code)")
    pairing_phone = digits

    with _lock:
        if is_running() and _mode == mode:
            return status()

        _stop_locked()
        _stop_existing_sidecars(BASE_DIR / "index.js")
        _purge_unregistered_session(_session_dir())

        index_js = BASE_DIR / "index.js"
        if not index_js.exists():
            raise RuntimeError(f"Relay sidecar missing: {index_js}")

        node_modules = BASE_DIR / "node_modules"
        if not node_modules.exists():
            raise RuntimeError(
                f"Sidecar deps not installed; run `cd {BASE_DIR} && npm install`"
            )

        try:
            _state_path().unlink()
        except FileNotFoundError:
            pass

        env = {
            **os.environ,
            "WA_SESSION_DIR": str(_session_dir()),
        }

        env["WA_PAIRING_PHONE"] = pairing_phone

        log_path = _data_dir() / "wa_relay.log"
        log_fd = os.open(str(log_path), os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)

        try:
            proc = subprocess.Popen(
                ["node", str(index_js)],
                cwd=str(BASE_DIR),
                stdout=log_fd,
                stderr=subprocess.STDOUT,
                stdin=subprocess.DEVNULL,
                env=env,
                start_new_session=True,
            )
        finally:
            os.close(log_fd)

        _proc = proc
        _mode = mode
        _started_at = time.time()

    # Wait briefly for the state file to be created
    deadline = time.time() + 3.0
    while time.time() < deadline:
        if _state_path().exists():
            break
        if not is_running():
            break
        time.sleep(0.1)

    return status()

def _iter_matching_sidecars(index_js: Path) -> set[int]:
    needle = str(index_js)
    current_pid = os.getpid()
    pids: set[int] = set()

    proc_dir = Path("/proc")
    if proc_dir.exists():
        for cmdline in proc_dir.glob("[0-9]*/cmdline"):
            try:
                pid = int(cmdline.parent.name)
                if pid == current_pid:
                    continue
                parts = cmdline.read_bytes().split(b"\0")
                argv0 = Path(parts[0].decode("utf-8", "ignore")).name if parts and parts[0] else ""
                command = b" ".join(parts).decode("utf-8", "ignore")
            except Exception:
                continue
            if argv0 == "node" and needle in command:
                pids.add(pid)
        return pids

    try:
        result = subprocess.run(
            ["pgrep", "-f", needle],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
        )
    except FileNotFoundError:
        return pids

    for line in result.stdout.splitlines():
        try:
            pid = int(line.strip())
        except ValueError:
            continue
        if pid != current_pid:
            pids.add(pid)
    return pids

def _stop_existing_sidecars(index_js: Path) -> None:
    pids = _iter_matching_sidecars(index_js)
    if not pids:
        return

    for pid in pids:
        try:
            os.kill(pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        except Exception:
            continue

    deadline = time.time() + 2.0
    while time.time() < deadline:
        alive = []
        for pid in pids:
            try:
                os.kill(pid, 0)
                alive.append(pid)
            except ProcessLookupError:
                pass
            except Exception:
                pass
        if not alive:
            return
        time.sleep(0.1)

    for pid in pids:
        try:
            os.kill(pid, signal.SIGKILL)
        except Exception:
            pass

def _purge_unregistered_session(session_dir: Path) -> None:
    """Clear session if it was only partially created."""
    creds = session_dir / "creds.json"
    if not creds.exists():
        return

    try:
        data = json.loads(creds.read_text(encoding="utf-8"))
    except Exception:
        shutil.rmtree(session_dir, ignore_errors=True)
        session_dir.mkdir(parents=True, exist_ok=True)
        return

    me = data.get("me") or {}
    account = data.get("account") or {}
    has_identity = bool(me.get("id")) and bool(account.get("deviceSignature"))

    if has_identity:
        return

    shutil.rmtree(session_dir, ignore_errors=True)
    session_dir.mkdir(parents=True, exist_ok=True)

def _stop_locked() -> None:
    global _proc, _mode, _started_at
    proc = _proc
    if proc is None:
        return

    if proc.poll() is None:
        try:
            os.killpg(os.getpgid(proc.pid), signal.SIGTERM)
        except Exception:
            try:
                proc.terminate()
            except Exception:
                pass

        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
            except Exception:
                proc.kill()
            proc.wait()

    _proc = None
    _mode = ""
    _started_at = None

def stop() -> dict:
    with _lock:
        _stop_locked()
    return status()

def logout() -> dict:
    """Stop sidecar AND remove session dir."""
    with _lock:
        _stop_locked()
        sd = _session_dir()
        if sd.exists():
            shutil.rmtree(sd, ignore_errors=True)
        sd.mkdir(parents=True, exist_ok=True)
        try:
            _state_path().unlink()
        except FileNotFoundError:
            pass
    return {"status": "logged_out"}

def get_otp(clear_after: bool = True) -> Optional[str]:
    raise RuntimeError("OTP retrieval is served by OtpService.WaitForOtp gRPC")
