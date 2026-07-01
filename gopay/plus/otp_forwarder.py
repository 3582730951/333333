#!/usr/bin/env python3
"""
OTP 转发器 — 通过 ADB 监听 Android 设备的 WhatsApp 通知，自动转发 GoPay OTP

使用方法：
  1. 确保 ADB 已连接到 Android 设备/模拟器：adb devices
  2. 修改下方 OTP_URL 和 AUTH 为你的编排器地址
  3. python3 otp_forwarder.py

注意：WhatsApp 通知不能被点开/已读，否则无法被 ADB 捕获
"""
import subprocess
import re
import time
import json
import urllib.request
import sys

# ─── 配置 ───
OTP_URL = "http://127.0.0.1:8800/otp"  # 编排器地址
AUTH = ""  # 如果设置了 auth_token，填 "Bearer 你的token"
ADB = "adb"  # ADB 路径，如果不在 PATH 里写完整路径

# ─── 方案 A：logcat 实时监听（推荐） ───
def run_logcat():
    print("[OTP转发器] 启动 logcat 监听模式...")
    print(f"[OTP转发器] 目标: {OTP_URL}")
    print("[OTP转发器] 按 Ctrl+C 停止\n")

    proc = subprocess.Popen(
        [ADB, "logcat", "-s", "NotificationService:I", "StatusBarNotification:I"],
        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True
    )

    sent_otps = set()
    for line in proc.stdout:
        if "whatsapp" not in line.lower() and "com.whatsapp" not in line.lower():
            continue
        match = re.search(r'\b(\d{6})\b', line)
        if not match:
            continue
        otp = match.group(1)
        if otp in sent_otps:
            continue
        sent_otps.add(otp)
        if len(sent_otps) > 20:
            sent_otps.pop()

        print(f"[{time.strftime('%H:%M:%S')}] 捕获 OTP: {otp}")
        push_otp(otp)


# ─── 方案 B：轮询 dumpsys notification（备选） ───
def run_poll():
    print("[OTP转发器] 启动轮询模式（每 2 秒检查一次通知栏）...")
    print(f"[OTP转发器] 目标: {OTP_URL}")
    print("[OTP转发器] 按 Ctrl+C 停止\n")

    sent_otps = set()
    while True:
        try:
            out = subprocess.check_output(
                [ADB, "shell", "dumpsys", "notification", "--noredact"],
                text=True, timeout=5
            )
            in_wa = False
            for line in out.splitlines():
                if "com.whatsapp" in line:
                    in_wa = True
                elif "pkg=" in line and "com.whatsapp" not in line:
                    in_wa = False
                if in_wa:
                    match = re.search(r'\b(\d{6})\b', line)
                    if match:
                        otp = match.group(1)
                        if otp not in sent_otps:
                            sent_otps.add(otp)
                            print(f"[{time.strftime('%H:%M:%S')}] 捕获 OTP: {otp}")
                            push_otp(otp)
        except Exception as e:
            print(f"[{time.strftime('%H:%M:%S')}] 错误: {e}")
        time.sleep(2)


def push_otp(otp):
    """推送 OTP 到编排器"""
    try:
        data = json.dumps({"otp": otp}).encode()
        headers = {"Content-Type": "application/json"}
        if AUTH:
            headers["Authorization"] = AUTH
        req = urllib.request.Request(OTP_URL, data=data, headers=headers)
        with urllib.request.urlopen(req, timeout=5) as r:
            resp = json.loads(r.read())
            print(f"  → 转发成功: {resp}")
    except Exception as e:
        print(f"  → 转发失败: {e}")


if __name__ == "__main__":
    mode = sys.argv[1] if len(sys.argv) > 1 else "logcat"
    try:
        if mode == "poll":
            run_poll()
        else:
            run_logcat()
    except KeyboardInterrupt:
        print("\n[OTP转发器] 已停止")
