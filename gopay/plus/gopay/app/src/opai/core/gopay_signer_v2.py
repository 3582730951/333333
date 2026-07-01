"""
GoPay V2 Signing — Complete Verified Implementation

Verified against Frida inline HMAC capture on 2026-05-07:
  - Inner SHA-256 hash: MATCH
  - Full HMAC-SHA256: MATCH
  - Message format: MATCH (with non-empty token)
  - Live API test: 200 OK on all endpoints with requests/httpx/tls_client

Message format (semicolon+colon pair separators):
  ;{model}:{token};{uniqueid}:{d1};{body_hash}:{url};{method}:{ts};{os}:{ver};{xm1}:{appid};{nonce}:{phone_make};{os_name}

Usage:
    from opai.core.gopay_signer_v2 import sign_v2

    result = sign_v2(
        token="Bearer eyJ...",
        timestamp_ms="1778088988941",
        url="customer.gopayapi.com/v1/linkedapps",
        method="GET",
    )
    # result["X-E1"], result["X-E2"], result["X-E3"]
"""
import hashlib
import hmac as hmac_mod
import os
import time

_DEFAULT_KEY_HEX = os.environ.get("GOPAY_V2_HMAC_KEY_HEX", "")
_V2_ID = os.environ.get("GOPAY_V2_ID", "CHANGE_ME_GOPAY_V2_ID")


def _default_hmac_key() -> bytes:
    if not _DEFAULT_KEY_HEX or _DEFAULT_KEY_HEX.startswith("CHANGE_ME"):
        raise RuntimeError("missing required runtime config: GOPAY_V2_HMAC_KEY_HEX")
    return bytes.fromhex(_DEFAULT_KEY_HEX)


def sign_v2(
    token: str = "",
    timestamp_ms: str = None,
    url: str = "",
    method: str = "GET",
    body: str = "",
    d1: str = "",
    model: str = "google,sdk_gphone64_x86_64",
    xm1: str = "",
    os_info: str = "Android,13",
    appid: str = "com.gojek.app",
    version: str = "5.60.1",
    adjts: str = "N",
    uniqueid: str = "",
    hmac_key: bytes = None,
    nonce_hex: str = None,
    phone_make: str = "Google",
    os_name: str = "Android",
) -> dict:
    """Sign a GoPay API request using the V2 algorithm.

    Returns dict with X-E1, X-E2, X-E3, and debug fields (_hmac, _message).
    """
    if token.startswith("Bearer "):
        token = token[7:]

    if timestamp_ms is None:
        timestamp_ms = str(int(time.time() * 1000))

    body_hash = hashlib.md5(body.encode("utf-8")).hexdigest()

    if nonce_hex is None:
        nonce_hex = os.urandom(80).hex()

    if hmac_key is None:
        hmac_key = _default_hmac_key()

    message = (
        f";{model}"
        f":{token}"
        f";{uniqueid}"
        f":{d1}"
        f";{body_hash}"
        f":{url}"
        f";{method}"
        f":{timestamp_ms}"
        f";{os_info}"
        f":{version}"
        f";{xm1}"
        f":{appid}"
        f";{nonce_hex}"
        f":{phone_make}"
        f";{os_name}"
    )

    hmac_hex = hmac_mod.new(
        hmac_key, message.encode("utf-8"), hashlib.sha256
    ).hexdigest()

    x_e1 = f"{hmac_hex}:{nonce_hex}:{adjts}:{timestamp_ms}"

    return {
        "X-E1": x_e1,
        "X-E2": _V2_ID,
        "X-E3": body_hash,
        "_hmac": hmac_hex,
        "_message": message,
    }
