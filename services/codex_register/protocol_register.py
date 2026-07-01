"""
ChatGPT 批量自动注册工具 (并发版)
依赖: pip install curl_cffi requests
功能: 临时邮箱 + 并发自动注册 ChatGPT 账号 + OTP 验证 + Codex OAuth Token
"""

import os
import re
import uuid
import json
import random
import string
import time
import threading
import secrets
import hashlib
import base64
from concurrent.futures import ThreadPoolExecutor, as_completed
from urllib.parse import urlparse, parse_qs, urlencode

from datetime import datetime, timezone, timedelta
import requests as std_requests
from curl_cffi import requests as curl_requests
import urllib3
urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)

# ================= 加载配置 =================
def _load_config():
    """从 config.json 加载配置，环境变量优先级更高"""
    config = {
        "total_accounts": 3,
        "email_provider": "mailtm",
        "duckmail_api_base": "https://api.duckmail.sbs",
        "duckmail_bearer": "",
        "proxy": "",
        "output_file": "registered_accounts.txt",
        "enable_oauth": True,
        "oauth_required": True,
        "oauth_issuer": "https://auth.openai.com",
        "oauth_client_id": "app_EMoamEEZ73f0CkXaXp7hrann",
        "oauth_redirect_uri": "http://localhost:1455/auth/callback",
        "ak_file": "ak.txt",
        "rk_file": "rk.txt",
        "token_json_dir": "codex_tokens",
        "upload_api_url": "",
        "upload_api_token": "",
    }

    config_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "config.json")
    if os.path.exists(config_path):
        try:
            with open(config_path, "r", encoding="utf-8") as f:
                file_config = json.load(f)
                config.update(file_config)
        except Exception as e:
            print(f"⚠️ 加载 config.json 失败: {e}")

    # 环境变量优先级更高
    config["duckmail_api_base"] = os.environ.get("DUCKMAIL_API_BASE", config["duckmail_api_base"])
    config["duckmail_bearer"] = os.environ.get("DUCKMAIL_BEARER", config["duckmail_bearer"])
    config["proxy"] = os.environ.get("PROXY", config["proxy"])
    config["total_accounts"] = int(os.environ.get("TOTAL_ACCOUNTS", config["total_accounts"]))
    config["enable_oauth"] = os.environ.get("ENABLE_OAUTH", config["enable_oauth"])
    config["oauth_required"] = os.environ.get("OAUTH_REQUIRED", config["oauth_required"])
    config["oauth_issuer"] = os.environ.get("OAUTH_ISSUER", config["oauth_issuer"])
    config["oauth_client_id"] = os.environ.get("OAUTH_CLIENT_ID", config["oauth_client_id"])
    config["oauth_redirect_uri"] = os.environ.get("OAUTH_REDIRECT_URI", config["oauth_redirect_uri"])
    config["ak_file"] = os.environ.get("AK_FILE", config["ak_file"])
    config["rk_file"] = os.environ.get("RK_FILE", config["rk_file"])
    config["token_json_dir"] = os.environ.get("TOKEN_JSON_DIR", config["token_json_dir"])
    config["upload_api_url"] = os.environ.get("UPLOAD_API_URL", config["upload_api_url"])
    config["upload_api_token"] = os.environ.get("UPLOAD_API_TOKEN", config["upload_api_token"])
    config["email_provider"] = os.environ.get("EMAIL_PROVIDER", config["email_provider"])

    return config


def _as_bool(value):
    if isinstance(value, bool):
        return value
    if value is None:
        return False
    return str(value).strip().lower() in {"1", "true", "yes", "y", "on"}


_CONFIG = _load_config()
EMAIL_PROVIDER = _CONFIG.get("email_provider", "mailtm").lower().strip()
DUCKMAIL_API_BASE = _CONFIG["duckmail_api_base"]
DUCKMAIL_BEARER = _CONFIG["duckmail_bearer"]
DEFAULT_TOTAL_ACCOUNTS = _CONFIG["total_accounts"]
DEFAULT_PROXY = _CONFIG["proxy"]
DEFAULT_OUTPUT_FILE = _CONFIG["output_file"]
ENABLE_OAUTH = _as_bool(_CONFIG.get("enable_oauth", True))
OAUTH_REQUIRED = _as_bool(_CONFIG.get("oauth_required", True))
OAUTH_ISSUER = _CONFIG["oauth_issuer"].rstrip("/")
OAUTH_CLIENT_ID = _CONFIG["oauth_client_id"]
OAUTH_REDIRECT_URI = _CONFIG["oauth_redirect_uri"]
AK_FILE = _CONFIG["ak_file"]
RK_FILE = _CONFIG["rk_file"]
TOKEN_JSON_DIR = _CONFIG["token_json_dir"]
UPLOAD_API_URL = _CONFIG["upload_api_url"]
UPLOAD_API_TOKEN = _CONFIG["upload_api_token"]

# 多邮箱提供商支持: mailtm / mailgw / duckmail / tempmail_lol / hotmail_otp
_EMAIL_PROVIDER_API = {
    "mailtm": "https://api.mail.tm",
    "mailgw": "https://api.mail.gw",
    "duckmail": DUCKMAIL_API_BASE,
    "tempmail_lol": "https://api.tempmail.lol",
    "hotmail_otp": "",
}

# hotmail_otp: Hotmail plus-addressing on a REAL inbox (OpenAI delivers to it, unlike
# disposable domains). All kvmlqqi+{tag}@hotmail.com land in one inbox; an external reader
# returns recent emails as JSON. (other_project's proven path — see docs/init.md.)
HOTMAIL_BASE_EMAIL = os.environ.get("HOTMAIL_BASE_EMAIL", "kvmlqqi@hotmail.com")
HOTMAIL_OTP_URL = os.environ.get(
    "HOTMAIL_OTP_URL",
    "http://185.242.234.133:8000/get_email/a89ae03149b94598a554614fa4d5e826",
)

if EMAIL_PROVIDER == "duckmail" and not DUCKMAIL_BEARER:
    print("⚠️ 警告: 使用 DuckMail 但未设置 DUCKMAIL_BEARER")
    print("   文件: config.json -> duckmail_bearer")
    print("   环境变量: export DUCKMAIL_BEARER='your_api_key_here'")
elif EMAIL_PROVIDER in ("mailtm", "mailgw"):
    print(f"[Info] 使用免费临时邮箱: {EMAIL_PROVIDER} ({_EMAIL_PROVIDER_API[EMAIL_PROVIDER]})")

# 全局线程锁
_print_lock = threading.Lock()
_file_lock = threading.Lock()
# 并发模式下精简日志（并发数>1时自动切换）
_VERBOSE = True


# Chrome 指纹配置: impersonate 与 sec-ch-ua 必须匹配真实浏览器
_CHROME_PROFILES = [
    {
        "major": 131, "impersonate": "chrome131",
        "build": 6778, "patch_range": (69, 205),
        "sec_ch_ua": '"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"',
    },
    {
        "major": 133, "impersonate": "chrome133a",
        "build": 6943, "patch_range": (33, 153),
        "sec_ch_ua": '"Not(A:Brand";v="99", "Google Chrome";v="133", "Chromium";v="133"',
    },
    {
        "major": 136, "impersonate": "chrome136",
        "build": 7103, "patch_range": (48, 175),
        "sec_ch_ua": '"Chromium";v="136", "Google Chrome";v="136", "Not.A/Brand";v="99"',
    },
]


def _random_chrome_version():
    profile = random.choice(_CHROME_PROFILES)
    major = profile["major"]
    build = profile["build"]
    patch = random.randint(*profile["patch_range"])
    full_ver = f"{major}.0.{build}.{patch}"
    ua = f"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/{full_ver} Safari/537.36"
    return profile["impersonate"], major, full_ver, ua, profile["sec_ch_ua"]


def _random_delay(low=0.3, high=1.0):
    time.sleep(random.uniform(low, high))


def _make_trace_headers():
    trace_id = random.randint(10**17, 10**18 - 1)
    parent_id = random.randint(10**17, 10**18 - 1)
    tp = f"00-{uuid.uuid4().hex}-{format(parent_id, '016x')}-01"
    return {
        "traceparent": tp, "tracestate": "dd=s:1;o:rum",
        "x-datadog-origin": "rum", "x-datadog-sampling-priority": "1",
        "x-datadog-trace-id": str(trace_id), "x-datadog-parent-id": str(parent_id),
    }


def _generate_pkce():
    code_verifier = base64.urlsafe_b64encode(secrets.token_bytes(64)).rstrip(b"=").decode("ascii")
    digest = hashlib.sha256(code_verifier.encode("ascii")).digest()
    code_challenge = base64.urlsafe_b64encode(digest).rstrip(b"=").decode("ascii")
    return code_verifier, code_challenge


class SentinelTokenGenerator:
    """纯 Python 版本 sentinel token 生成器（PoW）"""

    MAX_ATTEMPTS = 500000
    ERROR_PREFIX = "wQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D"

    def __init__(self, device_id=None, user_agent=None):
        self.device_id = device_id or str(uuid.uuid4())
        self.user_agent = user_agent or (
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
            "AppleWebKit/537.36 (KHTML, like Gecko) "
            "Chrome/145.0.0.0 Safari/537.36"
        )
        self.requirements_seed = str(random.random())
        self.sid = str(uuid.uuid4())

    @staticmethod
    def _fnv1a_32(text: str):
        h = 2166136261
        for ch in text:
            h ^= ord(ch)
            h = (h * 16777619) & 0xFFFFFFFF
        h ^= (h >> 16)
        h = (h * 2246822507) & 0xFFFFFFFF
        h ^= (h >> 13)
        h = (h * 3266489909) & 0xFFFFFFFF
        h ^= (h >> 16)
        h &= 0xFFFFFFFF
        return format(h, "08x")

    def _get_config(self):
        now_str = time.strftime(
            "%a %b %d %Y %H:%M:%S GMT+0000 (Coordinated Universal Time)",
            time.gmtime(),
        )
        perf_now = random.uniform(1000, 50000)
        time_origin = time.time() * 1000 - perf_now
        nav_prop = random.choice([
            "vendorSub", "productSub", "vendor", "maxTouchPoints",
            "scheduling", "userActivation", "doNotTrack", "geolocation",
            "connection", "plugins", "mimeTypes", "pdfViewerEnabled",
            "webkitTemporaryStorage", "webkitPersistentStorage",
            "hardwareConcurrency", "cookieEnabled", "credentials",
            "mediaDevices", "permissions", "locks", "ink",
        ])
        nav_val = f"{nav_prop}-undefined"

        return [
            "1920x1080",
            now_str,
            4294705152,
            random.random(),
            self.user_agent,
            "https://sentinel.openai.com/sentinel/20260124ceb8/sdk.js",
            None,
            None,
            "en-US",
            "en-US,en",
            random.random(),
            nav_val,
            random.choice(["location", "implementation", "URL", "documentURI", "compatMode"]),
            random.choice(["Object", "Function", "Array", "Number", "parseFloat", "undefined"]),
            perf_now,
            self.sid,
            "",
            random.choice([4, 8, 12, 16]),
            time_origin,
        ]

    @staticmethod
    def _base64_encode(data):
        raw = json.dumps(data, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        return base64.b64encode(raw).decode("ascii")

    def _run_check(self, start_time, seed, difficulty, config, nonce):
        config[3] = nonce
        config[9] = round((time.time() - start_time) * 1000)
        data = self._base64_encode(config)
        hash_hex = self._fnv1a_32(seed + data)
        diff_len = len(difficulty)
        if hash_hex[:diff_len] <= difficulty:
            return data + "~S"
        return None

    def generate_token(self, seed=None, difficulty=None):
        seed = seed if seed is not None else self.requirements_seed
        difficulty = str(difficulty or "0")
        start_time = time.time()
        config = self._get_config()

        for i in range(self.MAX_ATTEMPTS):
            result = self._run_check(start_time, seed, difficulty, config, i)
            if result:
                return "gAAAAAB" + result
        return "gAAAAAB" + self.ERROR_PREFIX + self._base64_encode(str(None))

    def generate_requirements_token(self):
        config = self._get_config()
        config[3] = 1
        config[9] = round(random.uniform(5, 50))
        data = self._base64_encode(config)
        return "gAAAAAC" + data


def fetch_sentinel_challenge(session, device_id, flow="authorize_continue", user_agent=None,
                             sec_ch_ua=None, impersonate=None):
    generator = SentinelTokenGenerator(device_id=device_id, user_agent=user_agent)
    req_body = {
        "p": generator.generate_requirements_token(),
        "id": device_id,
        "flow": flow,
    }
    headers = {
        "Content-Type": "text/plain;charset=UTF-8",
        "Referer": "https://sentinel.openai.com/backend-api/sentinel/frame.html",
        "Origin": "https://sentinel.openai.com",
        "User-Agent": user_agent or "Mozilla/5.0",
        "sec-ch-ua": sec_ch_ua or '"Not:A-Brand";v="99", "Google Chrome";v="145", "Chromium";v="145"',
        "sec-ch-ua-mobile": "?0",
        "sec-ch-ua-platform": '"Windows"',
    }

    kwargs = {
        "data": json.dumps(req_body),
        "headers": headers,
        "timeout": 20,
    }
    if impersonate:
        kwargs["impersonate"] = impersonate

    try:
        resp = session.post("https://sentinel.openai.com/backend-api/sentinel/req", **kwargs)
    except Exception:
        return None

    if resp.status_code != 200:
        return None

    try:
        return resp.json()
    except Exception:
        return None


def build_sentinel_token(session, device_id, flow="authorize_continue", user_agent=None,
                         sec_ch_ua=None, impersonate=None):
    challenge = fetch_sentinel_challenge(
        session,
        device_id,
        flow=flow,
        user_agent=user_agent,
        sec_ch_ua=sec_ch_ua,
        impersonate=impersonate,
    )
    if not challenge:
        return None

    c_value = challenge.get("token", "")
    if not c_value:
        return None

    pow_data = challenge.get("proofofwork") or {}
    generator = SentinelTokenGenerator(device_id=device_id, user_agent=user_agent)

    if pow_data.get("required") and pow_data.get("seed"):
        p_value = generator.generate_token(
            seed=pow_data.get("seed"),
            difficulty=pow_data.get("difficulty", "0"),
        )
    else:
        p_value = generator.generate_requirements_token()

    return json.dumps({
        "p": p_value,
        "t": "",
        "c": c_value,
        "id": device_id,
        "flow": flow,
    }, separators=(",", ":"))


def _extract_code_from_url(url: str):
    if not url or "code=" not in url:
        return None
    try:
        return parse_qs(urlparse(url).query).get("code", [None])[0]
    except Exception:
        return None


def _decode_jwt_payload(token: str):
    try:
        parts = token.split(".")
        if len(parts) != 3:
            return {}
        payload = parts[1]
        padding = 4 - len(payload) % 4
        if padding != 4:
            payload += "=" * padding
        decoded = base64.urlsafe_b64decode(payload)
        return json.loads(decoded)
    except Exception:
        return {}


def _save_codex_tokens(email: str, tokens: dict):
    access_token = tokens.get("access_token", "")
    refresh_token = tokens.get("refresh_token", "")
    id_token = tokens.get("id_token", "")

    if access_token:
        with _file_lock:
            with open(AK_FILE, "a", encoding="utf-8") as f:
                f.write(f"{access_token}\n")

    if refresh_token:
        with _file_lock:
            with open(RK_FILE, "a", encoding="utf-8") as f:
                f.write(f"{refresh_token}\n")

    if not access_token:
        return

    payload = _decode_jwt_payload(access_token)
    auth_info = payload.get("https://api.openai.com/auth", {})
    account_id = auth_info.get("chatgpt_account_id", "")

    _tz8 = timezone(timedelta(hours=8))
    exp_timestamp = payload.get("exp")
    expired_str = ""
    if isinstance(exp_timestamp, int) and exp_timestamp > 0:
        expired_str = datetime.fromtimestamp(exp_timestamp, tz=_tz8).strftime("%Y-%m-%dT%H:%M:%S+08:00")

    now = datetime.now(tz=_tz8)
    token_data = {
        "type": "codex",
        "email": email,
        "expired": expired_str,
        "id_token": id_token,
        "account_id": account_id,
        "access_token": access_token,
        "last_refresh": now.strftime("%Y-%m-%dT%H:%M:%S+08:00"),
        "refresh_token": refresh_token,
    }

    base_dir = os.path.dirname(os.path.abspath(__file__))
    token_dir = TOKEN_JSON_DIR if os.path.isabs(TOKEN_JSON_DIR) else os.path.join(base_dir, TOKEN_JSON_DIR)
    os.makedirs(token_dir, exist_ok=True)

    # Machine-readable marker so a parent process (pool_server Go) can import the account.
    print("__CODEX_ACCOUNT__ " + json.dumps(token_data, ensure_ascii=False), flush=True)

    token_path = os.path.join(token_dir, f"{email}.json")
    with _file_lock:
        with open(token_path, "w", encoding="utf-8") as f:
            json.dump(token_data, f, ensure_ascii=False)

    # 上传到 CPA 管理平台
    if UPLOAD_API_URL:
        _upload_token_json(token_path)


def _upload_token_json(filepath):
    """上传 Token JSON 文件到 CPA 管理平台"""
    mp = None
    try:
        from curl_cffi import CurlMime

        filename = os.path.basename(filepath)
        mp = CurlMime()
        mp.addpart(
            name="file",
            content_type="application/json",
            filename=filename,
            local_path=filepath,
        )

        session = curl_requests.Session()
        if DEFAULT_PROXY:
            session.proxies = {"http": DEFAULT_PROXY, "https": DEFAULT_PROXY}

        resp = session.post(
            UPLOAD_API_URL,
            multipart=mp,
            headers={"Authorization": f"Bearer {UPLOAD_API_TOKEN}"},
            verify=False,
            timeout=30,
        )

        if resp.status_code == 200:
            with _print_lock:
                print(f"  [CPA] Token JSON 已上传到 CPA 管理平台")
        else:
            with _print_lock:
                print(f"  [CPA] 上传失败: {resp.status_code} - {resp.text[:200]}")
    except Exception as e:
        with _print_lock:
            print(f"  [CPA] 上传异常: {e}")
    finally:
        if mp:
            mp.close()


def _generate_password(length=14):
    lower = string.ascii_lowercase
    upper = string.ascii_uppercase
    digits = string.digits
    special = "!@#$%&*"
    pwd = [random.choice(lower), random.choice(upper),
           random.choice(digits), random.choice(special)]
    all_chars = lower + upper + digits + special
    pwd += [random.choice(all_chars) for _ in range(length - 4)]
    random.shuffle(pwd)
    return "".join(pwd)


def _random_name():
    first = random.choice([
        "James", "Emma", "Liam", "Olivia", "Noah", "Ava", "Ethan", "Sophia",
        "Lucas", "Mia", "Mason", "Isabella", "Logan", "Charlotte", "Alexander",
        "Amelia", "Benjamin", "Harper", "William", "Evelyn", "Henry", "Abigail",
        "Sebastian", "Emily", "Jack", "Elizabeth",
    ])
    last = random.choice([
        "Smith", "Johnson", "Brown", "Davis", "Wilson", "Moore", "Taylor",
        "Clark", "Hall", "Young", "Anderson", "Thomas", "Jackson", "White",
        "Harris", "Martin", "Thompson", "Garcia", "Robinson", "Lewis",
        "Walker", "Allen", "King", "Wright", "Scott", "Green",
    ])
    return f"{first} {last}"


def _random_birthdate():
    y = random.randint(1985, 2002)
    m = random.randint(1, 12)
    d = random.randint(1, 28)
    return f"{y}-{m:02d}-{d:02d}"


class ChatGPTRegister:
    BASE = "https://chatgpt.com"
    AUTH = "https://auth.openai.com"

    def __init__(self, proxy: str = None, tag: str = ""):
        self.tag = tag  # 线程标识，用于日志
        self.device_id = str(uuid.uuid4())
        self.auth_session_logging_id = str(uuid.uuid4())
        self.impersonate, self.chrome_major, self.chrome_full, self.ua, self.sec_ch_ua = _random_chrome_version()

        self.session = curl_requests.Session(impersonate=self.impersonate)

        self.proxy = proxy
        if self.proxy:
            self.session.proxies = {"http": self.proxy, "https": self.proxy}

        self.session.headers.update({
            "User-Agent": self.ua,
            "Accept-Language": random.choice([
                "en-US,en;q=0.9", "en-US,en;q=0.9,zh-CN;q=0.8",
                "en,en-US;q=0.9", "en-US,en;q=0.8",
            ]),
            "sec-ch-ua": self.sec_ch_ua, "sec-ch-ua-mobile": "?0",
            "sec-ch-ua-platform": '"Windows"', "sec-ch-ua-arch": '"x86"',
            "sec-ch-ua-bitness": '"64"',
            "sec-ch-ua-full-version": f'"{self.chrome_full}"',
            "sec-ch-ua-platform-version": f'"{random.randint(10, 15)}.0.0"',
        })

        self.session.cookies.set("oai-did", self.device_id, domain="chatgpt.com")
        self._callback_url = None
        self._final_callback_url = None  # callback 最终跳转 URL

    def _log(self, step, method, url, status, body=None):
        prefix = f"[{self.tag}] " if self.tag else ""
        if _VERBOSE:
            lines = [
                f"\n{'='*60}",
                f"{prefix}[Step] {step}",
                f"{prefix}[{method}] {url}",
                f"{prefix}[Status] {status}",
            ]
            if body:
                try:
                    lines.append(f"{prefix}[Response] {json.dumps(body, indent=2, ensure_ascii=False)[:1000]}")
                except Exception:
                    lines.append(f"{prefix}[Response] {str(body)[:1000]}")
            lines.append(f"{'='*60}")
            with _print_lock:
                print("\n".join(lines))
        else:
            # 并发精简模式：只输出步骤+状态码
            with _print_lock:
                print(f"    {prefix}{step} -> {status}")

    def _print(self, msg):
        prefix = f"[{self.tag}] " if self.tag else ""
        if _VERBOSE:
            with _print_lock:
                print(f"{prefix}{msg}")

    # ==================== 临时邮箱（多提供商） ====================

    def _create_mail_session(self):
        """创建临时邮箱 API 请求会话。注意：邮箱 API 一律直连，不走住宅代理——
        住宅 IP 访问临时邮箱服务不稳定/会被限流，且没有理由代理收信流量。"""
        session = curl_requests.Session(impersonate=self.impersonate)
        session.headers.update({
            "User-Agent": self.ua,
            "Accept": "application/json",
            "Content-Type": "application/json",
        })
        # 邮箱不走代理（直连）
        return session

    def _get_mail_api_base(self):
        """根据 EMAIL_PROVIDER 返回对应 API base URL"""
        return _EMAIL_PROVIDER_API.get(EMAIL_PROVIDER, "https://api.mail.tm").rstrip("/")

    def _fetch_available_domain(self, session, api_base):
        """从 mail.tm / mail.gw 获取可用邮箱域名"""
        res = session.get(f"{api_base}/domains", timeout=15, verify=False)
        if res.status_code != 200:
            raise Exception(f"获取域名列表失败: {res.status_code}")
        data = res.json()
        # 兼容两种返回格式: list 或 hydra collection dict
        if isinstance(data, list):
            members = data
        else:
            members = data.get("hydra:member") or data.get("member") or []
        active = [m for m in members if isinstance(m, dict) and m.get("isActive", True)]
        if not active:
            raise Exception("没有可用的邮箱域名")
        return random.choice(active)["domain"]

    def create_temp_email(self):
        """创建临时邮箱，返回 (email, password, mail_token)"""
        api_base = self._get_mail_api_base()
        password = _generate_password()

        try:
            # ====== hotmail_otp: 真实 Hotmail + plus-addressing（直连读信） ======
            if EMAIL_PROVIDER == "hotmail_otp":
                tag = "".join(random.choice(string.ascii_lowercase + string.digits) for _ in range(8))
                email = HOTMAIL_BASE_EMAIL.replace("@", f"+{tag}@")
                # 快照当前收件箱，后续只接受「之后」到达的新邮件，避免取到旧验证码
                self._hotmail_seen = set()
                try:
                    r = std_requests.get(HOTMAIL_OTP_URL, timeout=12, proxies=None, verify=False)
                    for m in (r.json().get("emails", []) if r.status_code == 200 else []):
                        self._hotmail_seen.add(str(m.get("时间", "")) + str(m.get("主题", "")))
                except Exception:
                    pass
                self._print(f"[Email] Hotmail plus-addressing: {email} (基线 {len(self._hotmail_seen)} 封)")
                return email, password, "hotmail_otp"

            # ====== tempmail.lol: 完全不同的 API（无需账号密码） ======
            if EMAIL_PROVIDER == "tempmail_lol":
                res = std_requests.post(
                    f"{api_base}/v2/inbox/create",
                    timeout=15,
                    proxies=None,  # 邮箱 API 直连，不走住宅代理
                    verify=False,
                )
                if res.status_code not in [200, 201]:
                    raise Exception(f"创建收件箱失败: {res.status_code} - {res.text[:200]}")
                data = res.json()
                email = data.get("address", "")
                mail_token = data.get("token", "")
                if not email or not mail_token:
                    raise Exception(f"返回数据不完整: {data}")
                domain = email.split("@")[-1]
                self._print(f"[Email] tempmail.lol 域名: {domain}")
                return email, password, mail_token

            # ====== 其他提供商: 标准 accounts + token API ======
            session = self._create_mail_session()
            chars = string.ascii_lowercase + string.digits
            length = random.randint(8, 13)
            email_local = "".join(random.choice(chars) for _ in range(length))

            if EMAIL_PROVIDER in ("mailtm", "mailgw"):
                domain = self._fetch_available_domain(session, api_base)
                email = f"{email_local}@{domain}"
                self._print(f"[Email] 使用 {EMAIL_PROVIDER} 域名: {domain}")
                headers = {}
            else:
                if not DUCKMAIL_BEARER:
                    raise Exception("DUCKMAIL_BEARER 未设置")
                email = f"{email_local}@duckmail.sbs"
                headers = {"Authorization": f"Bearer {DUCKMAIL_BEARER}"}

            payload = {"address": email, "password": password}
            res = session.post(
                f"{api_base}/accounts",
                json=payload,
                headers=headers,
                timeout=15,
                verify=False,
            )
            if res.status_code not in [200, 201]:
                raise Exception(f"创建邮箱失败: {res.status_code} - {res.text[:200]}")

            time.sleep(0.5)
            token_res = session.post(
                f"{api_base}/token",
                json={"address": email, "password": password},
                timeout=15,
                verify=False,
            )
            if token_res.status_code == 200:
                mail_token = token_res.json().get("token")
                if mail_token:
                    return email, password, mail_token
            raise Exception(f"获取邮件 Token 失败: {token_res.status_code}")

        except Exception as e:
            raise Exception(f"[{EMAIL_PROVIDER}] 创建邮箱失败: {e}")

    def _fetch_emails(self, mail_token: str):
        """获取邮件列表（兼容所有提供商）"""
        try:
            api_base = self._get_mail_api_base()

            # hotmail_otp: 直连外部读信服务，只返回基线之后到达的「新」邮件
            if EMAIL_PROVIDER == "hotmail_otp":
                res = std_requests.get(HOTMAIL_OTP_URL, timeout=15, proxies=None, verify=False)
                if res.status_code != 200:
                    return []
                seen = getattr(self, "_hotmail_seen", set())
                out = []
                for m in res.json().get("emails", []):
                    key = str(m.get("时间", "")) + str(m.get("主题", ""))
                    if key in seen:
                        continue
                    out.append(m)
                return out

            # tempmail.lol: 用标准 requests + token 查询参数
            if EMAIL_PROVIDER == "tempmail_lol":
                res = std_requests.get(
                    f"{api_base}/v2/inbox",
                    params={"token": mail_token},
                    timeout=15,
                    proxies=None,  # 邮箱 API 直连，不走住宅代理
                    verify=False,
                )
                if res.status_code == 200:
                    data = res.json()
                    # tempmail.lol 返回 {emails: [...]} 列表，每个邮件自带 body/subject
                    return data.get("emails") or data.get("data") or []
                return []

            # 其他提供商: bearer token
            headers = {"Authorization": f"Bearer {mail_token}"}
            session = self._create_mail_session()
            res = session.get(
                f"{api_base}/messages",
                headers=headers,
                timeout=15,
                verify=False,
            )
            if res.status_code == 200:
                data = res.json()
                messages = data.get("hydra:member") or data.get("member") or data.get("data") or []
                return messages
            return []
        except Exception:
            return []

    def _fetch_email_detail(self, mail_token: str, msg_id: str):
        """获取单封邮件详情（mail.tm / mail.gw / duckmail 用，tempmail_lol 不需要）"""
        try:
            api_base = self._get_mail_api_base()

            # tempmail.lol: 邮件内容直接在列表中返回（body 字段），不需要再请求详情
            if EMAIL_PROVIDER == "tempmail_lol":
                return None  # tempmail.lol 在 _fetch_emails 中已返回完整邮件

            headers = {"Authorization": f"Bearer {mail_token}"}
            session = self._create_mail_session()
            if isinstance(msg_id, str) and msg_id.startswith("/messages/"):
                msg_id = msg_id.split("/")[-1]
            res = session.get(
                f"{api_base}/messages/{msg_id}",
                headers=headers,
                timeout=15,
                verify=False,
            )
            if res.status_code == 200:
                return res.json()
        except Exception:
            pass
        return None

    def _extract_verification_code(self, email_content: str):
        """从邮件内容提取 6 位验证码"""
        if not email_content:
            return None

        patterns = [
            r"Verification code:?\s*(\d{6})",
            r"code is\s*(\d{6})",
            r"代码为[:：]?\s*(\d{6})",
            r"验证码[:：]?\s*(\d{6})",
            r">\s*(\d{6})\s*<",
            r"(?<![#&])\b(\d{6})\b",
        ]

        for pattern in patterns:
            matches = re.findall(pattern, email_content, re.IGNORECASE)
            for code in matches:
                if code == "177010":  # 已知误判
                    continue
                return code
        return None

    def _get_msg_content(self, mail_token: str, msg: dict) -> str:
        """从邮件消息中提取正文（兼容所有提供商）"""
        if EMAIL_PROVIDER == "hotmail_otp":
            return " ".join([str(msg.get("内容预览", "")), str(msg.get("html", "")),
                             str(msg.get("主题", "")), str(msg.get("subject", "")),
                             str(msg.get("body", ""))])
        if EMAIL_PROVIDER == "tempmail_lol":
            return msg.get("body", "") or msg.get("text", "") or msg.get("html", "")
        msg_id = msg.get("id") or msg.get("@id")
        if not msg_id:
            return ""
        detail = self._fetch_email_detail(mail_token, msg_id)
        return (detail.get("text") or detail.get("html") or "") if detail else ""

    def _scan_otp_from_messages(self, mail_token: str) -> str:
        """扫描邮件列表，提取第一个有效的 OTP 验证码"""
        messages = self._fetch_emails(mail_token)
        if not isinstance(messages, list):
            return None
        for msg in messages[:12]:
            if not isinstance(msg, dict):
                continue
            content = self._get_msg_content(mail_token, msg)
            if content:
                code = self._extract_verification_code(content)
                if code:
                    return code
        return None

    def wait_for_verification_email(self, mail_token: str, timeout: int = 130):
        """等待并提取 OpenAI 验证码"""
        self._print(f"[OTP] 等待验证码邮件 (最多 {timeout}s)...")
        start_time = time.time()

        while time.time() - start_time < timeout:
            code = self._scan_otp_from_messages(mail_token)
            if code:
                self._print(f"[OTP] 验证码: {code}")
                return code
            elapsed = int(time.time() - start_time)
            self._print(f"[OTP] 等待中... ({elapsed}s/{timeout}s)")
            time.sleep(5)

        self._print(f"[OTP] 超时 ({timeout}s)")
        return None

    # ==================== 注册流程 ====================

    def visit_homepage(self):
        url = f"{self.BASE}/"
        r = self.session.get(url, headers={
            "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
            "Upgrade-Insecure-Requests": "1",
        }, allow_redirects=True)
        self._log("0. Visit homepage", "GET", url, r.status_code,
                   {"cookies_count": len(self.session.cookies)})

    def get_csrf(self) -> str:
        url = f"{self.BASE}/api/auth/csrf"
        r = self.session.get(url, headers={"Accept": "application/json", "Referer": f"{self.BASE}/"})
        data = r.json()
        token = data.get("csrfToken", "")
        self._log("1. Get CSRF", "GET", url, r.status_code, data)
        if not token:
            raise Exception("Failed to get CSRF token")
        return token

    def signin(self, email: str, csrf: str) -> str:
        url = f"{self.BASE}/api/auth/signin/openai"
        params = {
            "prompt": "login", "ext-oai-did": self.device_id,
            "auth_session_logging_id": self.auth_session_logging_id,
            "screen_hint": "login_or_signup", "login_hint": email,
        }
        form_data = {"callbackUrl": f"{self.BASE}/", "csrfToken": csrf, "json": "true"}
        r = self.session.post(url, params=params, data=form_data, headers={
            "Content-Type": "application/x-www-form-urlencoded",
            "Accept": "application/json", "Referer": f"{self.BASE}/", "Origin": self.BASE,
        })
        data = r.json()
        authorize_url = data.get("url", "")
        self._log("2. Signin", "POST", url, r.status_code, data)
        if not authorize_url:
            raise Exception("Failed to get authorize URL")
        return authorize_url

    def authorize(self, url: str) -> str:
        r = self.session.get(url, headers={
            "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
            "Referer": f"{self.BASE}/", "Upgrade-Insecure-Requests": "1",
        }, allow_redirects=True)
        final_url = str(r.url)
        self._log("3. Authorize", "GET", url, r.status_code, {"final_url": final_url})
        return final_url

    def register(self, email: str, password: str, sentinel_token: str = None):
        url = f"{self.AUTH}/api/accounts/user/register"
        headers = {"Content-Type": "application/json", "Accept": "application/json",
                    "Referer": f"{self.AUTH}/create-account/password", "Origin": self.AUTH}
        headers.update(_make_trace_headers())
        if sentinel_token:
            headers["openai-sentinel-token"] = sentinel_token
        r = self.session.post(url, json={"username": email, "password": password}, headers=headers)
        try: data = r.json()
        except Exception: data = {"text": r.text[:500]}
        self._log("4. Register", "POST", url, r.status_code, data)
        return r.status_code, data

    def send_otp(self):
        url = f"{self.AUTH}/api/accounts/email-otp/send"
        r = self.session.get(url, headers={
            "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
            "Referer": f"{self.AUTH}/create-account/password", "Upgrade-Insecure-Requests": "1",
        }, allow_redirects=True)
        try: data = r.json()
        except Exception: data = {"final_url": str(r.url), "status": r.status_code}
        self._log("5. Send OTP", "GET", url, r.status_code, data)
        return r.status_code, data

    def validate_otp(self, code: str, sentinel_token: str = None):
        url = f"{self.AUTH}/api/accounts/email-otp/validate"
        headers = {"Content-Type": "application/json", "Accept": "application/json",
                    "Referer": f"{self.AUTH}/email-verification", "Origin": self.AUTH}
        headers.update(_make_trace_headers())
        if sentinel_token:
            headers["openai-sentinel-token"] = sentinel_token
        r = self.session.post(url, json={"code": code}, headers=headers)
        try: data = r.json()
        except Exception: data = {"text": r.text[:500]}
        self._log("6. Validate OTP", "POST", url, r.status_code, data)
        return r.status_code, data

    def create_account(self, name: str, birthdate: str, so_token: str = None):
        url = f"{self.AUTH}/api/accounts/create_account"
        headers = {
            "Content-Type": "application/json",
            "Accept": "application/json",
            "Referer": f"{self.AUTH}/about-you",
            "Origin": self.AUTH,
            "User-Agent": self.ua,
            "oai-device-id": self.device_id,
            "sec-ch-ua": self.sec_ch_ua,
            "sec-ch-ua-mobile": "?0",
            "sec-ch-ua-platform": '"Windows"',
            "sec-fetch-dest": "empty",
            "sec-fetch-mode": "cors",
            "sec-fetch-site": "same-origin",
        }
        headers.update(_make_trace_headers())

        payload = {"name": name, "birthdate": birthdate}

        # 始终带 sentinel token（OpenAI 可能已强制要求）
        if so_token:
            headers["openai-sentinel-token"] = so_token

        r = self.session.post(url, json=payload, headers=headers, impersonate=self.impersonate)

        # 如果 400 registration_disallowed，尝试重新获取 sentinel token 后重试
        if r.status_code == 400 and "registration_disallowed" in (r.text or ""):
            self._print("[Create] registration_disallowed，重新获取 sentinel 重试...")
            fresh_token = build_sentinel_token(
                self.session, self.device_id, flow="oauth_create_account",
                user_agent=self.ua, sec_ch_ua=self.sec_ch_ua, impersonate=self.impersonate,
            )
            if fresh_token:
                headers["openai-sentinel-token"] = fresh_token
                r = self.session.post(url, json=payload, headers=headers, impersonate=self.impersonate)

        try: data = r.json()
        except Exception: data = {"text": r.text[:500]}
        self._log("7. Create Account", "POST", url, r.status_code, data)
        if isinstance(data, dict):
            cb = data.get("continue_url") or data.get("url") or data.get("redirect_url")
            if cb:
                self._callback_url = cb
        return r.status_code, data

    def callback(self, url: str = None):
        if not url:
            url = self._callback_url
        if not url:
            self._print("[!] No callback URL, skipping.")
            return None, None
        r = self.session.get(url, headers={
            "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
            "Upgrade-Insecure-Requests": "1",
        }, allow_redirects=True)
        final_url = str(r.url)
        self._final_callback_url = final_url
        self._log("8. Callback", "GET", url, r.status_code, {"final_url": final_url})
        return r.status_code, {"final_url": final_url}

    def get_chatgpt_session_tokens(self):
        """从 ChatGPT session 接口提取 token（复用注册后的会话）"""
        self._print("[Session] 尝试从 ChatGPT session 获取 token...")
        try:
            referer = self._final_callback_url or f"{self.BASE}/"
            r = self.session.get(
                f"{self.BASE}/api/auth/session",
                headers={
                    "Accept": "application/json",
                    "Referer": referer,
                    "User-Agent": self.ua,
                },
                timeout=30,
                impersonate=self.impersonate,
            )
            self._print(f"[Session] /api/auth/session -> {r.status_code}")
            if r.status_code != 200:
                return None

            data = r.json()
            if not isinstance(data, dict):
                return None

            # 优先读取已知字段名
            access_token = data.get("accessToken") or data.get("access_token") or ""
            if not access_token:
                # 递归扫描 JSON 找 JWT
                access_token = self._find_jwt_in_data(data)

            if not access_token:
                self._print("[Session] session 中未找到 accessToken")
                return None

            self._print("[Session] 成功从 session 提取 accessToken")
            return {
                "access_token": access_token,
                "refresh_token": data.get("refreshToken") or data.get("refresh_token") or "",
                "id_token": data.get("idToken") or data.get("id_token") or "",
            }
        except Exception as e:
            self._print(f"[Session] 获取 session 异常: {e}")
            return None

    @staticmethod
    def _find_jwt_in_data(data, depth=0):
        """递归扫描 dict/list，找到第一个看起来像 JWT 的字符串"""
        if depth > 5:
            return None
        if isinstance(data, str):
            parts = data.split(".")
            if len(parts) == 3 and len(data) > 100:
                try:
                    payload = parts[1]
                    padding = 4 - len(payload) % 4
                    if padding != 4:
                        payload += "=" * padding
                    decoded = base64.urlsafe_b64decode(payload)
                    obj = json.loads(decoded)
                    if isinstance(obj, dict) and ("exp" in obj or "iat" in obj or "sub" in obj):
                        return data
                except Exception:
                    pass
            return None
        if isinstance(data, dict):
            for v in data.values():
                result = ChatGPTRegister._find_jwt_in_data(v, depth + 1)
                if result:
                    return result
        if isinstance(data, list):
            for item in data:
                result = ChatGPTRegister._find_jwt_in_data(item, depth + 1)
                if result:
                    return result
        return None

    # ==================== 自动注册主流程 ====================

    def _fetch_sentinel_tokens(self):
        """一次性获取两个 sentinel token（含 PoW 求解）"""
        # 1. authorize_continue 用于 register / validate_otp 等步骤
        sentinel_token = build_sentinel_token(
            self.session, self.device_id, flow="authorize_continue",
            user_agent=self.ua, sec_ch_ua=self.sec_ch_ua, impersonate=self.impersonate,
        )
        if sentinel_token:
            self._print(f"[Sentinel] authorize_continue token 已获取 (含 PoW)")
        else:
            self._print(f"[Sentinel] authorize_continue token 获取失败")

        # 2. oauth_create_account 用于 create_account 步骤
        so_token = build_sentinel_token(
            self.session, self.device_id, flow="oauth_create_account",
            user_agent=self.ua, sec_ch_ua=self.sec_ch_ua, impersonate=self.impersonate,
        )
        if so_token:
            self._print(f"[Sentinel] oauth_create_account SO token 已获取 (含 PoW)")
        else:
            self._print(f"[Sentinel] oauth_create_account SO token 获取失败")

        return sentinel_token, so_token

    def run_register(self, email, password, name, birthdate, mail_token):
        """注册流程（支持所有邮箱提供商）"""
        self.visit_homepage()
        _random_delay(0.3, 0.8)
        csrf = self.get_csrf()
        _random_delay(0.2, 0.5)
        auth_url = self.signin(email, csrf)
        _random_delay(0.3, 0.8)

        final_url = self.authorize(auth_url)
        final_path = urlparse(final_url).path
        _random_delay(0.3, 0.8)

        self._print(f"Authorize → {final_path}")

        # 获取两个 sentinel token（authorize 后、register 前）
        sentinel_token, so_token = self._fetch_sentinel_tokens()

        need_otp = False

        if "create-account/password" in final_path:
            self._print("全新注册流程")
            _random_delay(0.5, 1.0)
            status, data = self.register(email, password, sentinel_token=sentinel_token)
            if status != 200:
                raise Exception(f"Register 失败 ({status}): {data}")
            _random_delay(0.3, 0.8)
            self.send_otp()
            need_otp = True
        elif "email-verification" in final_path or "email-otp" in final_path:
            self._print("跳到 OTP 验证阶段 (authorize 已触发 OTP，不再重复发送)")
            need_otp = True
        elif "about-you" in final_path:
            self._print("跳到填写信息阶段")
            _random_delay(0.5, 1.0)
            self.create_account(name, birthdate, so_token=so_token)
            _random_delay(0.3, 0.5)
            self.callback()
            return True
        elif "callback" in final_path or "chatgpt.com" in final_url:
            self._print("账号已完成注册")
            return True
        else:
            self._print(f"未知跳转: {final_url}")
            self.register(email, password, sentinel_token=sentinel_token)
            self.send_otp()
            need_otp = True

        if need_otp:
            otp_code = self.wait_for_verification_email(mail_token)
            if not otp_code:
                raise Exception("未能获取验证码")

            _random_delay(0.3, 0.8)
            status, data = self.validate_otp(otp_code, sentinel_token=sentinel_token)
            if status != 200:
                self._print("验证码失败，重试...")
                self.send_otp()
                _random_delay(1.0, 2.0)
                otp_code = self.wait_for_verification_email(mail_token, timeout=60)
                if not otp_code:
                    raise Exception("重试后仍未获取验证码")
                _random_delay(0.3, 0.8)
                status, data = self.validate_otp(otp_code, sentinel_token=sentinel_token)
                if status != 200:
                    raise Exception(f"验证码失败 ({status}): {data}")

        # 跟随 continue_url 访问 about-you 页面（模拟浏览器导航）
        continue_url = ""
        if isinstance(data, dict):
            continue_url = data.get("continue_url", "")
        if not continue_url:
            continue_url = f"{self.AUTH}/about-you"
        if continue_url.startswith("/"):
            continue_url = f"{self.AUTH}{continue_url}"

        _random_delay(0.5, 1.0)
        self._print(f"[Flow] GET {continue_url}")
        try:
            r = self.session.get(continue_url, headers={
                "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
                "Referer": f"{self.AUTH}/email-verification",
                "Upgrade-Insecure-Requests": "1",
                "User-Agent": self.ua,
            }, allow_redirects=True, impersonate=self.impersonate)
            self._log("6.5 Visit about-you", "GET", continue_url, r.status_code,
                       {"final_url": str(r.url)})
        except Exception as e:
            self._print(f"[Flow] 访问 about-you 异常（继续尝试）: {e}")

        _random_delay(0.5, 1.5)
        status, data = self.create_account(name, birthdate, so_token=so_token)
        if status != 200:
            raise Exception(f"Create account 失败 ({status}): {data}")
        _random_delay(0.2, 0.5)
        self.callback()
        return True

    def _decode_oauth_session_cookie(self):
        jar = getattr(self.session.cookies, "jar", None)
        if jar is not None:
            cookie_items = list(jar)
        else:
            cookie_items = []

        for c in cookie_items:
            name = getattr(c, "name", "") or ""
            if "oai-client-auth-session" not in name:
                continue

            raw_val = (getattr(c, "value", "") or "").strip()
            if not raw_val:
                continue

            candidates = [raw_val]
            try:
                from urllib.parse import unquote

                decoded = unquote(raw_val)
                if decoded != raw_val:
                    candidates.append(decoded)
            except Exception:
                pass

            for val in candidates:
                try:
                    if (val.startswith('"') and val.endswith('"')) or (val.startswith("'") and val.endswith("'")):
                        val = val[1:-1]

                    part = val.split(".")[0] if "." in val else val
                    pad = 4 - len(part) % 4
                    if pad != 4:
                        part += "=" * pad
                    raw = base64.urlsafe_b64decode(part)
                    data = json.loads(raw.decode("utf-8"))
                    if isinstance(data, dict):
                        return data
                except Exception:
                    continue
        return None

    def _oauth_allow_redirect_extract_code(self, url: str, referer: str = None):
        headers = {
            "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
            "Upgrade-Insecure-Requests": "1",
            "User-Agent": self.ua,
        }
        if referer:
            headers["Referer"] = referer

        try:
            resp = self.session.get(
                url,
                headers=headers,
                allow_redirects=True,
                timeout=30,
                impersonate=self.impersonate,
            )
            final_url = str(resp.url)
            code = _extract_code_from_url(final_url)
            if code:
                self._print("[OAuth] allow_redirect 命中最终 URL code")
                return code

            for r in getattr(resp, "history", []) or []:
                loc = r.headers.get("Location", "")
                code = _extract_code_from_url(loc)
                if code:
                    self._print("[OAuth] allow_redirect 命中 history Location code")
                    return code
                code = _extract_code_from_url(str(r.url))
                if code:
                    self._print("[OAuth] allow_redirect 命中 history URL code")
                    return code
        except Exception as e:
            maybe_localhost = re.search(r'(https?://localhost[^\s\'\"]+)', str(e))
            if maybe_localhost:
                code = _extract_code_from_url(maybe_localhost.group(1))
                if code:
                    self._print("[OAuth] allow_redirect 从 localhost 异常提取 code")
                    return code
            self._print(f"[OAuth] allow_redirect 异常: {e}")

        return None

    def _oauth_follow_for_code(self, start_url: str, referer: str = None, max_hops: int = 16):
        headers = {
            "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
            "Upgrade-Insecure-Requests": "1",
            "User-Agent": self.ua,
        }
        if referer:
            headers["Referer"] = referer

        current_url = start_url
        last_url = start_url

        for hop in range(max_hops):
            try:
                resp = self.session.get(
                    current_url,
                    headers=headers,
                    allow_redirects=False,
                    timeout=30,
                    impersonate=self.impersonate,
                )
            except Exception as e:
                maybe_localhost = re.search(r'(https?://localhost[^\s\'\"]+)', str(e))
                if maybe_localhost:
                    code = _extract_code_from_url(maybe_localhost.group(1))
                    if code:
                        self._print(f"[OAuth] follow[{hop + 1}] 命中 localhost 回调")
                        return code, maybe_localhost.group(1)
                self._print(f"[OAuth] follow[{hop + 1}] 请求异常: {e}")
                return None, last_url

            last_url = str(resp.url)
            self._print(f"[OAuth] follow[{hop + 1}] {resp.status_code} {last_url[:140]}")
            code = _extract_code_from_url(last_url)
            if code:
                return code, last_url

            if resp.status_code in (301, 302, 303, 307, 308):
                loc = resp.headers.get("Location", "")
                if not loc:
                    return None, last_url
                if loc.startswith("/"):
                    loc = f"{OAUTH_ISSUER}{loc}"
                code = _extract_code_from_url(loc)
                if code:
                    return code, loc
                current_url = loc
                headers["Referer"] = last_url
                continue

            return None, last_url

        return None, last_url

    def _oauth_submit_workspace_and_org(self, consent_url: str):
        session_data = self._decode_oauth_session_cookie()
        if not session_data:
            jar = getattr(self.session.cookies, "jar", None)
            if jar is not None:
                cookie_names = [getattr(c, "name", "") for c in list(jar)]
            else:
                cookie_names = list(self.session.cookies.keys())
            self._print(f"[OAuth] 无法解码 oai-client-auth-session, cookies={cookie_names[:12]}")
            return None

        workspaces = session_data.get("workspaces", [])
        if not workspaces:
            self._print("[OAuth] session 中没有 workspace 信息")
            return None

        workspace_id = (workspaces[0] or {}).get("id")
        if not workspace_id:
            self._print("[OAuth] workspace_id 为空")
            return None

        h = {
            "Accept": "application/json",
            "Content-Type": "application/json",
            "Origin": OAUTH_ISSUER,
            "Referer": consent_url,
            "User-Agent": self.ua,
            "oai-device-id": self.device_id,
        }
        h.update(_make_trace_headers())

        resp = self.session.post(
            f"{OAUTH_ISSUER}/api/accounts/workspace/select",
            json={"workspace_id": workspace_id},
            headers=h,
            allow_redirects=False,
            timeout=30,
            impersonate=self.impersonate,
        )
        self._print(f"[OAuth] workspace/select -> {resp.status_code}")

        if resp.status_code in (301, 302, 303, 307, 308):
            loc = resp.headers.get("Location", "")
            if loc.startswith("/"):
                loc = f"{OAUTH_ISSUER}{loc}"
            code = _extract_code_from_url(loc)
            if code:
                return code
            code, _ = self._oauth_follow_for_code(loc, referer=consent_url)
            if not code:
                code = self._oauth_allow_redirect_extract_code(loc, referer=consent_url)
            return code

        if resp.status_code != 200:
            self._print(f"[OAuth] workspace/select 失败: {resp.status_code}")
            return None

        try:
            ws_data = resp.json()
        except Exception:
            self._print("[OAuth] workspace/select 响应不是 JSON")
            return None

        ws_next = ws_data.get("continue_url", "")
        orgs = ws_data.get("data", {}).get("orgs", [])
        ws_page = (ws_data.get("page") or {}).get("type", "")
        self._print(f"[OAuth] workspace/select page={ws_page or '-'} next={(ws_next or '-')[:140]}")

        org_id = None
        project_id = None
        if orgs:
            org_id = (orgs[0] or {}).get("id")
            projects = (orgs[0] or {}).get("projects", [])
            if projects:
                project_id = (projects[0] or {}).get("id")

        if org_id:
            org_body = {"org_id": org_id}
            if project_id:
                org_body["project_id"] = project_id

            h_org = dict(h)
            if ws_next:
                h_org["Referer"] = ws_next if ws_next.startswith("http") else f"{OAUTH_ISSUER}{ws_next}"

            resp_org = self.session.post(
                f"{OAUTH_ISSUER}/api/accounts/organization/select",
                json=org_body,
                headers=h_org,
                allow_redirects=False,
                timeout=30,
                impersonate=self.impersonate,
            )
            self._print(f"[OAuth] organization/select -> {resp_org.status_code}")
            if resp_org.status_code in (301, 302, 303, 307, 308):
                loc = resp_org.headers.get("Location", "")
                if loc.startswith("/"):
                    loc = f"{OAUTH_ISSUER}{loc}"
                code = _extract_code_from_url(loc)
                if code:
                    return code
                code, _ = self._oauth_follow_for_code(loc, referer=h_org.get("Referer"))
                if not code:
                    code = self._oauth_allow_redirect_extract_code(loc, referer=h_org.get("Referer"))
                return code

            if resp_org.status_code == 200:
                try:
                    org_data = resp_org.json()
                except Exception:
                    self._print("[OAuth] organization/select 响应不是 JSON")
                    return None

                org_next = org_data.get("continue_url", "")
                org_page = (org_data.get("page") or {}).get("type", "")
                self._print(f"[OAuth] organization/select page={org_page or '-'} next={(org_next or '-')[:140]}")
                if org_next:
                    if org_next.startswith("/"):
                        org_next = f"{OAUTH_ISSUER}{org_next}"
                    code, _ = self._oauth_follow_for_code(org_next, referer=h_org.get("Referer"))
                    if not code:
                        code = self._oauth_allow_redirect_extract_code(org_next, referer=h_org.get("Referer"))
                    return code

        if ws_next:
            if ws_next.startswith("/"):
                ws_next = f"{OAUTH_ISSUER}{ws_next}"
            code, _ = self._oauth_follow_for_code(ws_next, referer=consent_url)
            if not code:
                code = self._oauth_allow_redirect_extract_code(ws_next, referer=consent_url)
            return code

        return None

    def perform_codex_oauth_login_http(self, email: str, password: str, mail_token: str = None):
        self._print("[OAuth] 开始执行 Codex OAuth 纯协议流程...")

        # 兼容两种 domain 形式，确保 auth 域也带 oai-did
        self.session.cookies.set("oai-did", self.device_id, domain=".auth.openai.com")
        self.session.cookies.set("oai-did", self.device_id, domain="auth.openai.com")

        code_verifier, code_challenge = _generate_pkce()
        state = secrets.token_urlsafe(24)

        authorize_params = {
            "response_type": "code",
            "client_id": OAUTH_CLIENT_ID,
            "redirect_uri": OAUTH_REDIRECT_URI,
            "scope": "openid profile email offline_access",
            "code_challenge": code_challenge,
            "code_challenge_method": "S256",
            "state": state,
        }
        authorize_url = f"{OAUTH_ISSUER}/oauth/authorize?{urlencode(authorize_params)}"

        def _oauth_json_headers(referer: str):
            h = {
                "Accept": "application/json",
                "Content-Type": "application/json",
                "Origin": OAUTH_ISSUER,
                "Referer": referer,
                "User-Agent": self.ua,
                "oai-device-id": self.device_id,
            }
            h.update(_make_trace_headers())
            return h

        def _bootstrap_oauth_session():
            self._print("[OAuth] 1/7 GET /oauth/authorize")
            try:
                r = self.session.get(
                    authorize_url,
                    headers={
                        "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
                        "Referer": f"{self.BASE}/",
                        "Upgrade-Insecure-Requests": "1",
                        "User-Agent": self.ua,
                    },
                    allow_redirects=True,
                    timeout=30,
                    impersonate=self.impersonate,
                )
            except Exception as e:
                self._print(f"[OAuth] /oauth/authorize 异常: {e}")
                return False, ""

            final_url = str(r.url)
            redirects = len(getattr(r, "history", []) or [])
            self._print(f"[OAuth] /oauth/authorize -> {r.status_code}, final={(final_url or '-')[:140]}, redirects={redirects}")

            has_login = any(getattr(c, "name", "") == "login_session" for c in self.session.cookies)
            self._print(f"[OAuth] login_session: {'已获取' if has_login else '未获取'}")

            if not has_login:
                self._print("[OAuth] 未拿到 login_session，尝试访问 oauth2 auth 入口")
                oauth2_url = f"{OAUTH_ISSUER}/api/oauth/oauth2/auth"
                try:
                    r2 = self.session.get(
                        oauth2_url,
                        headers={
                            "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
                            "Referer": authorize_url,
                            "Upgrade-Insecure-Requests": "1",
                            "User-Agent": self.ua,
                        },
                        params=authorize_params,
                        allow_redirects=True,
                        timeout=30,
                        impersonate=self.impersonate,
                    )
                    final_url = str(r2.url)
                    redirects2 = len(getattr(r2, "history", []) or [])
                    self._print(f"[OAuth] /api/oauth/oauth2/auth -> {r2.status_code}, final={(final_url or '-')[:140]}, redirects={redirects2}")
                except Exception as e:
                    self._print(f"[OAuth] /api/oauth/oauth2/auth 异常: {e}")

                has_login = any(getattr(c, "name", "") == "login_session" for c in self.session.cookies)
                self._print(f"[OAuth] login_session(重试): {'已获取' if has_login else '未获取'}")

            return has_login, final_url

        def _post_authorize_continue(referer_url: str):
            sentinel_authorize = build_sentinel_token(
                self.session,
                self.device_id,
                flow="authorize_continue",
                user_agent=self.ua,
                sec_ch_ua=self.sec_ch_ua,
                impersonate=self.impersonate,
            )
            if not sentinel_authorize:
                self._print("[OAuth] authorize_continue 的 sentinel token 获取失败")
                return None

            headers_continue = _oauth_json_headers(referer_url)
            headers_continue["openai-sentinel-token"] = sentinel_authorize

            try:
                return self.session.post(
                    f"{OAUTH_ISSUER}/api/accounts/authorize/continue",
                    json={"username": {"kind": "email", "value": email}},
                    headers=headers_continue,
                    timeout=30,
                    allow_redirects=False,
                    impersonate=self.impersonate,
                )
            except Exception as e:
                self._print(f"[OAuth] authorize/continue 异常: {e}")
                return None

        has_login_session, authorize_final_url = _bootstrap_oauth_session()
        if not authorize_final_url:
            return None

        continue_referer = authorize_final_url if authorize_final_url.startswith(OAUTH_ISSUER) else f"{OAUTH_ISSUER}/log-in"

        self._print("[OAuth] 2/7 POST /api/accounts/authorize/continue")
        resp_continue = _post_authorize_continue(continue_referer)
        if resp_continue is None:
            return None

        self._print(f"[OAuth] /authorize/continue -> {resp_continue.status_code}")
        if resp_continue.status_code == 400 and "invalid_auth_step" in (resp_continue.text or ""):
            self._print("[OAuth] invalid_auth_step，重新 bootstrap 后重试一次")
            has_login_session, authorize_final_url = _bootstrap_oauth_session()
            if not authorize_final_url:
                return None
            continue_referer = authorize_final_url if authorize_final_url.startswith(OAUTH_ISSUER) else f"{OAUTH_ISSUER}/log-in"
            resp_continue = _post_authorize_continue(continue_referer)
            if resp_continue is None:
                return None
            self._print(f"[OAuth] /authorize/continue(重试) -> {resp_continue.status_code}")

        if resp_continue.status_code != 200:
            self._print(f"[OAuth] 邮箱提交失败: {resp_continue.text[:180]}")
            return None

        try:
            continue_data = resp_continue.json()
        except Exception:
            self._print("[OAuth] authorize/continue 响应解析失败")
            return None

        continue_url = continue_data.get("continue_url", "")
        page_type = (continue_data.get("page") or {}).get("type", "")
        self._print(f"[OAuth] continue page={page_type or '-'} next={(continue_url or '-')[:140]}")

        # 根据 authorize/continue 返回的 page_type 分支处理
        need_oauth_otp = (
            page_type == "email_otp_verification"
            or "email-verification" in (continue_url or "")
            or "email-otp" in (continue_url or "")
        )

        # 只有 login_password 或未知类型才走 password/verify
        skip_password = need_oauth_otp or page_type in (
            "create_account_password", "email_otp_verification",
            "consent", "organization_select",
        )

        if not skip_password:
            self._print("[OAuth] 3/7 POST /api/accounts/password/verify")
            sentinel_pwd = build_sentinel_token(
                self.session,
                self.device_id,
                flow="password_verify",
                user_agent=self.ua,
                sec_ch_ua=self.sec_ch_ua,
                impersonate=self.impersonate,
            )
            if not sentinel_pwd:
                self._print("[OAuth] password_verify 的 sentinel token 获取失败")
                return None

            headers_verify = _oauth_json_headers(f"{OAUTH_ISSUER}/log-in/password")
            headers_verify["openai-sentinel-token"] = sentinel_pwd

            try:
                resp_verify = self.session.post(
                    f"{OAUTH_ISSUER}/api/accounts/password/verify",
                    json={"password": password},
                    headers=headers_verify,
                    timeout=30,
                    allow_redirects=False,
                    impersonate=self.impersonate,
                )
            except Exception as e:
                self._print(f"[OAuth] password/verify 异常: {e}")
                return None

            self._print(f"[OAuth] /password/verify -> {resp_verify.status_code}")
            if resp_verify.status_code != 200:
                self._print(f"[OAuth] 密码校验失败: {resp_verify.text[:180]}")
                return None

            try:
                verify_data = resp_verify.json()
            except Exception:
                self._print("[OAuth] password/verify 响应解析失败")
                return None

            continue_url = verify_data.get("continue_url", "") or continue_url
            page_type = (verify_data.get("page") or {}).get("type", "") or page_type
            self._print(f"[OAuth] verify page={page_type or '-'} next={(continue_url or '-')[:140]}")

            # password/verify 之后也可能需要 OTP
            need_oauth_otp = (
                page_type == "email_otp_verification"
                or "email-verification" in (continue_url or "")
                or "email-otp" in (continue_url or "")
            )
        else:
            self._print(f"[OAuth] 3/7 跳过 password/verify (page_type={page_type})")

        if need_oauth_otp:
            self._print("[OAuth] 4/7 检测到邮箱 OTP 验证")
            if not mail_token:
                self._print("[OAuth] OAuth 阶段需要邮箱 OTP，但未提供 mail_token")
                return None

            headers_otp = _oauth_json_headers(f"{OAUTH_ISSUER}/email-verification")
            tried_codes = set()
            otp_success = False
            otp_deadline = time.time() + 130

            while time.time() < otp_deadline and not otp_success:
                messages = self._fetch_emails(mail_token) or []
                candidate_codes = []

                for msg in messages[:12]:
                    if not isinstance(msg, dict):
                        continue
                    content = self._get_msg_content(mail_token, msg)
                    if not content:
                        continue
                    code = self._extract_verification_code(content)
                    if code and code not in tried_codes:
                        candidate_codes.append(code)

                if not candidate_codes:
                    elapsed = int(time.time() - (otp_deadline - 130))
                    self._print(f"[OAuth] OTP 等待中... ({elapsed}s/130s)")
                    time.sleep(5)
                    continue

                for otp_code in candidate_codes:
                    tried_codes.add(otp_code)
                    self._print(f"[OAuth] 尝试 OTP: {otp_code}")
                    try:
                        resp_otp = self.session.post(
                            f"{OAUTH_ISSUER}/api/accounts/email-otp/validate",
                            json={"code": otp_code},
                            headers=headers_otp,
                            timeout=30,
                            allow_redirects=False,
                            impersonate=self.impersonate,
                        )
                    except Exception as e:
                        self._print(f"[OAuth] email-otp/validate 异常: {e}")
                        continue

                    self._print(f"[OAuth] /email-otp/validate -> {resp_otp.status_code}")
                    if resp_otp.status_code != 200:
                        self._print(f"[OAuth] OTP 无效，继续尝试下一条: {resp_otp.text[:160]}")
                        continue

                    try:
                        otp_data = resp_otp.json()
                    except Exception:
                        self._print("[OAuth] email-otp/validate 响应解析失败")
                        continue

                    continue_url = otp_data.get("continue_url", "") or continue_url
                    page_type = (otp_data.get("page") or {}).get("type", "") or page_type
                    self._print(f"[OAuth] OTP 验证通过 page={page_type or '-'} next={(continue_url or '-')[:140]}")
                    otp_success = True
                    break

                if not otp_success:
                    time.sleep(2)

            if not otp_success:
                self._print(f"[OAuth] OAuth 阶段 OTP 验证失败，已尝试 {len(tried_codes)} 个验证码")
                return None

        code = None
        consent_url = continue_url
        if consent_url and consent_url.startswith("/"):
            consent_url = f"{OAUTH_ISSUER}{consent_url}"

        if not consent_url and "consent" in page_type:
            consent_url = f"{OAUTH_ISSUER}/sign-in-with-chatgpt/codex/consent"

        if consent_url:
            code = _extract_code_from_url(consent_url)

        if not code and consent_url:
            self._print("[OAuth] 5/7 跟随 continue_url 提取 code")
            code, _ = self._oauth_follow_for_code(consent_url, referer=f"{OAUTH_ISSUER}/log-in/password")

        consent_hint = (
            ("consent" in (consent_url or ""))
            or ("sign-in-with-chatgpt" in (consent_url or ""))
            or ("workspace" in (consent_url or ""))
            or ("organization" in (consent_url or ""))
            or ("consent" in page_type)
            or ("organization" in page_type)
        )

        if not code and consent_hint:
            if not consent_url:
                consent_url = f"{OAUTH_ISSUER}/sign-in-with-chatgpt/codex/consent"
            self._print("[OAuth] 6/7 执行 workspace/org 选择")
            code = self._oauth_submit_workspace_and_org(consent_url)

        if not code:
            fallback_consent = f"{OAUTH_ISSUER}/sign-in-with-chatgpt/codex/consent"
            self._print("[OAuth] 6/7 回退 consent 路径重试")
            code = self._oauth_submit_workspace_and_org(fallback_consent)
            if not code:
                code, _ = self._oauth_follow_for_code(fallback_consent, referer=f"{OAUTH_ISSUER}/log-in/password")

        if not code:
            self._print("[OAuth] 未获取到 authorization code")
            return None

        self._print("[OAuth] 7/7 POST /oauth/token")
        token_resp = self.session.post(
            f"{OAUTH_ISSUER}/oauth/token",
            headers={"Content-Type": "application/x-www-form-urlencoded", "User-Agent": self.ua},
            data={
                "grant_type": "authorization_code",
                "code": code,
                "redirect_uri": OAUTH_REDIRECT_URI,
                "client_id": OAUTH_CLIENT_ID,
                "code_verifier": code_verifier,
            },
            timeout=60,
            impersonate=self.impersonate,
        )
        self._print(f"[OAuth] /oauth/token -> {token_resp.status_code}")

        if token_resp.status_code != 200:
            self._print(f"[OAuth] token 交换失败: {token_resp.status_code} {token_resp.text[:200]}")
            return None

        try:
            data = token_resp.json()
        except Exception:
            self._print("[OAuth] token 响应解析失败")
            return None

        if not data.get("access_token"):
            self._print("[OAuth] token 响应缺少 access_token")
            return None

        self._print("[OAuth] Codex Token 获取成功")
        return data


# ==================== 并发批量注册 ====================

# 全局统计（线程安全）
_stats_lock = threading.Lock()
_stats = {"success": 0, "fail": 0, "retry": 0, "running": 0}
_cancel_event = threading.Event()


def _update_stats(**kwargs):
    with _stats_lock:
        for k, v in kwargs.items():
            _stats[k] = _stats.get(k, 0) + v


def _print_progress(idx, total, email, status, detail=""):
    """统一的进度输出格式"""
    with _stats_lock:
        s, f = _stats["success"], _stats["fail"]
    elapsed_info = f"[{s}ok/{f}fail/{total}total]"
    symbol = {"ok": "+", "fail": "-", "retry": "~", "start": ">", "info": "*"}
    sym = symbol.get(status, "*")
    tag = email.split("@")[0] if email and "@" in email else f"#{idx}"
    detail_str = f" {detail}" if detail else ""
    with _print_lock:
        print(f"  [{sym}] {elapsed_info} [{tag}]{detail_str}")


def _register_one(idx, total, proxy, output_file, max_retries=3):
    """单个注册任务（线程内运行）- 自动重试邮箱创建和域名拦截"""
    if _cancel_event.is_set():
        return False, None, "cancelled"

    _update_stats(running=1)
    last_error = ""

    for attempt in range(1, max_retries + 1):
        if _cancel_event.is_set():
            _update_stats(running=-1)
            return False, None, "cancelled"

        reg = None
        try:
            reg = ChatGPTRegister(proxy=proxy, tag=f"{idx}")

            # 1. 创建临时邮箱（失败自动重试）
            email, email_pwd, mail_token = reg.create_temp_email()
            tag = email.split("@")[0]
            reg.tag = tag

            chatgpt_password = _generate_password()
            name = _random_name()
            birthdate = _random_birthdate()

            if attempt == 1:
                _print_progress(idx, total, email, "start",
                                f"注册中... ({EMAIL_PROVIDER})")
            else:
                _print_progress(idx, total, email, "retry",
                                f"第{attempt}次重试 (上次: {last_error[:60]})")

            # 2. 执行注册流程
            reg.run_register(email, chatgpt_password, name, birthdate, mail_token)

            # 3. OAuth（可选）- 优先复用注册 session，失败回退独立 OAuth
            oauth_ok = True
            if ENABLE_OAUTH:
                # 第一层：尝试从注册后的 ChatGPT session 直接提取 token
                tokens = reg.get_chatgpt_session_tokens()
                if tokens and tokens.get("access_token"):
                    _print_progress(idx, total, email, "info", "session 快捷路径成功")
                else:
                    # 第二层：回退到独立 OAuth 登录流程
                    _print_progress(idx, total, email, "info", "session 无 token，回退独立 OAuth")
                    tokens = reg.perform_codex_oauth_login_http(
                        email, chatgpt_password, mail_token=mail_token)
                oauth_ok = bool(tokens and tokens.get("access_token"))
                if oauth_ok:
                    _save_codex_tokens(email, tokens)
                else:
                    if OAUTH_REQUIRED:
                        raise Exception("OAuth 获取失败 (required=true)")

            # 4. 写入结果
            with _file_lock:
                with open(output_file, "a", encoding="utf-8") as out:
                    out.write(f"{email}----{chatgpt_password}----{email_pwd}"
                              f"----oauth={'ok' if oauth_ok else 'fail'}\n")

            _update_stats(success=1, running=-1)
            _print_progress(idx, total, email, "ok",
                            f"注册成功! (oauth={'ok' if oauth_ok else 'skip'})"
                            + (f" [第{attempt}次]" if attempt > 1 else ""))
            return True, email, None

        except Exception as e:
            last_error = str(e)
            is_domain_blocked = "registration_disallowed" in last_error
            is_retryable = is_domain_blocked or "创建邮箱失败" in last_error \
                          or "创建收件箱失败" in last_error or "未能获取验证码" in last_error \
                          or "TLS" in last_error or "timeout" in last_error.lower()

            if is_retryable and attempt < max_retries:
                _update_stats(retry=1)
                _print_progress(idx, total, None, "retry",
                                f"重试 {attempt}/{max_retries}: {last_error[:80]}")
                # 退避延迟：每次重试间隔递增
                time.sleep(random.uniform(1.0, 3.0) * attempt)
                continue
            else:
                break

    # 所有重试用尽
    _update_stats(fail=1, running=-1)
    _print_progress(idx, total, None, "fail",
                    f"失败 (尝试{max_retries}次): {last_error[:100]}")
    return False, None, last_error


def run_batch(total_accounts: int = 3, output_file="registered_accounts.txt",
              max_workers=3, proxy=None, max_retries=3):
    """并发批量注册 - 支持多邮箱提供商 + 自动重试 + 实时进度"""

    if EMAIL_PROVIDER == "duckmail" and not DUCKMAIL_BEARER:
        print("  错误: 使用 DuckMail 但未设置 DUCKMAIL_BEARER")
        print("  请切换邮箱: config.json -> email_provider: tempmail_lol")
        return

    actual_workers = min(max_workers, total_accounts)
    mail_api = _EMAIL_PROVIDER_API.get(EMAIL_PROVIDER, "unknown")

    # 并发数>1时自动精简日志
    global _VERBOSE
    _VERBOSE = (actual_workers <= 1)

    print(f"\n{'#'*60}")
    print(f"  ChatGPT 批量自动注册")
    print(f"  注册数量: {total_accounts} | 并发数: {actual_workers} | 重试: {max_retries}次")
    print(f"  邮箱: {EMAIL_PROVIDER} ({mail_api})")
    print(f"  OAuth: {'开启' if ENABLE_OAUTH else '关闭'}"
          f"{' (required)' if OAUTH_REQUIRED else ''}")
    if ENABLE_OAUTH:
        print(f"  Token输出: {TOKEN_JSON_DIR}/, {AK_FILE}, {RK_FILE}")
    print(f"  输出: {output_file}")
    print(f"{'#'*60}\n")

    # 重置全局统计
    with _stats_lock:
        _stats.update({"success": 0, "fail": 0, "retry": 0, "running": 0})
    _cancel_event.clear()

    start_time = time.time()
    completed = 0

    try:
        with ThreadPoolExecutor(max_workers=actual_workers) as executor:
            futures = {}
            # 分批提交，避免一次性创建过多任务
            batch_size = actual_workers * 2
            task_idx = 1

            while task_idx <= total_accounts and not _cancel_event.is_set():
                # 提交一批任务
                while len([f for f in futures if not f.done()]) < batch_size \
                      and task_idx <= total_accounts:
                    future = executor.submit(
                        _register_one, task_idx, total_accounts,
                        proxy, output_file, max_retries
                    )
                    futures[future] = task_idx
                    task_idx += 1
                    # 并发启动间隔，避免同时创建邮箱
                    time.sleep(random.uniform(0.5, 1.5))

                # 等待已完成的任务
                done_futures = [f for f in futures if f.done()]
                for future in done_futures:
                    if future in futures:
                        try:
                            future.result()
                        except Exception as e:
                            with _print_lock:
                                print(f"  [!] 线程异常: {e}")
                        completed += 1
                        del futures[future]

                if not done_futures:
                    time.sleep(0.5)

            # 等待剩余任务完成
            for future in as_completed(futures):
                try:
                    future.result()
                except Exception as e:
                    with _print_lock:
                        print(f"  [!] 线程异常: {e}")
                completed += 1

    except KeyboardInterrupt:
        print("\n\n  [!] 收到中断信号，正在停止...")
        _cancel_event.set()
        time.sleep(2)

    elapsed = time.time() - start_time
    with _stats_lock:
        s, f, r = _stats["success"], _stats["fail"], _stats["retry"]

    avg = elapsed / max(s, 1) if s > 0 else 0
    throughput = s / (elapsed / 60) if elapsed > 0 and s > 0 else 0

    print(f"\n{'#'*60}")
    print(f"  注册完成! 耗时 {elapsed:.1f}s")
    print(f"  成功: {s} | 失败: {f} | 重试: {r}次")
    if s > 0:
        print(f"  平均: {avg:.1f}s/个 | 吞吐: {throughput:.1f}个/分钟")
        print(f"  结果: {output_file}")
    if s + f < total_accounts:
        print(f"  跳过: {total_accounts - s - f} (取消)")
    print(f"{'#'*60}")


def main():
    print("=" * 60)
    print("  ChatGPT 批量自动注册工具")
    print(f"  邮箱: {EMAIL_PROVIDER} | OAuth: {'ON' if ENABLE_OAUTH else 'OFF'}")
    print("=" * 60)

    # 非交互模式（pool_server 子进程调用）：全部参数走 env/config，不读 stdin。
    if os.environ.get("REG_AUTO") == "1":
        proxy = DEFAULT_PROXY or os.environ.get("REG_PROXY") or None
        total = int(os.environ.get("REG_COUNT", "1") or "1")
        max_workers = int(os.environ.get("REG_WORKERS", "1") or "1")
        max_retries = int(os.environ.get("REG_RETRIES", "1") or "1")
        print(f"[Auto] proxy={proxy} count={total} workers={max_workers} retries={max_retries}")
        run_batch(total_accounts=total, output_file=DEFAULT_OUTPUT_FILE,
                  max_workers=max_workers, proxy=proxy, max_retries=max_retries)
        return

    # 邮箱配置检查
    if EMAIL_PROVIDER == "duckmail" and not DUCKMAIL_BEARER:
        print("\n  警告: DuckMail 需要 API Key，建议切换到 tempmail_lol")
        print("  修改: config.json -> email_provider: tempmail_lol")
        print("\n  按 Enter 继续...")
        input()

    # 代理配置
    proxy = DEFAULT_PROXY
    if proxy:
        print(f"\n[Info] 代理: {proxy}")
        choice = input("使用此代理? (Y/n): ").strip().lower()
        if choice == "n":
            proxy = input("输入代理地址 (留空=直连): ").strip() or None
    else:
        env_proxy = os.environ.get("HTTPS_PROXY") or os.environ.get("https_proxy") \
                 or os.environ.get("ALL_PROXY") or os.environ.get("all_proxy")
        if env_proxy:
            print(f"\n[Info] 环境变量代理: {env_proxy}")
            choice = input("使用此代理? (Y/n): ").strip().lower()
            proxy = None if choice == "n" else env_proxy
            if choice == "n":
                proxy = input("输入代理地址 (留空=直连): ").strip() or None
        else:
            proxy = input("\n代理地址 (如 http://127.0.0.1:7890，留空=直连): ").strip() or None

    print(f"[Info] {'代理: ' + proxy if proxy else '直连模式'}")

    # 注册参数
    count_input = input(f"\n注册数量 (默认 {DEFAULT_TOTAL_ACCOUNTS}): ").strip()
    total = int(count_input) if count_input.isdigit() and int(count_input) > 0 \
        else DEFAULT_TOTAL_ACCOUNTS

    workers_input = input("并发数 (默认 5): ").strip()
    max_workers = int(workers_input) if workers_input.isdigit() and int(workers_input) > 0 else 5

    retries_input = input("单账号最大重试次数 (默认 3): ").strip()
    max_retries = int(retries_input) if retries_input.isdigit() and int(retries_input) > 0 else 3

    run_batch(total_accounts=total, output_file=DEFAULT_OUTPUT_FILE,
              max_workers=max_workers, proxy=proxy, max_retries=max_retries)


if __name__ == "__main__":
    main()
