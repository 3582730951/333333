#!/usr/bin/env python3
"""
测试 sidecar 返回的响应头中是否包含 Content-Type: text/event-stream
"""
import base64
import json

# 模拟 sidecar 的 relay_response 逻辑
def simulate_sidecar_headers(upstream_headers):
    """模拟 sidecar 如何处理上游响应头"""
    reported = {}
    for k, v in upstream_headers.items():
        # sidecar 会过滤这些头
        if k.lower() in {"content-encoding", "content-length", "transfer-encoding"}:
            print(f"  ✗ Filtered out: {k}: {v}")
            continue
        reported[k] = [v]
        print(f"  ✓ Kept: {k}: {v}")

    return reported

print("=== 测试 1: Codex SSE 响应头 ===")
print("上游（chatgpt.com）返回的头:")
upstream_codex = {
    "Content-Type": "text/event-stream",
    "Cache-Control": "no-cache",
    "Connection": "keep-alive",
    "Transfer-Encoding": "chunked",
}

reported = simulate_sidecar_headers(upstream_codex)
print(f"\n编码后发送给 Go 的头: {reported}")

# 检查 Content-Type 是否保留
if "Content-Type" in reported or "content-type" in reported:
    print("✓ Content-Type: text/event-stream 被保留")
else:
    print("✗ Content-Type 丢失!")

print("\n" + "="*50)
print("=== 测试 2: sidecar 实际设置的响应头 ===")
print("sidecar 自己在 HTTP 响应中设置:")
print("  1. x-sidecar-upstream-status: 200")
print("  2. x-sidecar-upstream-headers-b64: <base64 of reported>")
print("  3. content-type: response.headers.get('content-type', 'application/octet-stream')")
print("  4. connection: close")

print("\n关键问题: sidecar 设置的 content-type 是直接从上游读取的")
print("如果上游是 text/event-stream，sidecar 也会设置 content-type: text/event-stream")
print("但是 Go 端会用 x-sidecar-upstream-headers-b64 中的头**替换**整个 header!")

print("\n" + "="*50)
print("=== 验证 Go 端的处理 ===")
print("Go upstream/client.go:578-594 的逻辑:")
print("""
    header := resp.Header.Clone()  // sidecar 的响应头
    if encoded := header.Get("x-sidecar-upstream-headers-b64"); encoded != "" {
        if decoded, err := base64.StdEncoding.DecodeString(encoded); err == nil {
            var upstreamHeaders http.Header
            if json.Unmarshal(decoded, &upstreamHeaders) == nil {
                header = upstreamHeaders  // 完全替换!
            }
        }
    }
    return &Response{StatusCode: resp.StatusCode, Header: header, Body: ...}
""")

print("\n结论: Go 使用的 header 应该包含 Content-Type: text/event-stream")
print("因为 sidecar 在 reported{} 中保留了它\n")

# 实际编码测试
print("=== 实际编码测试 ===")
reported_json = json.dumps({"Content-Type": ["text/event-stream"], "Cache-Control": ["no-cache"]})
encoded = base64.b64encode(reported_json.encode("utf-8")).decode("ascii")
print(f"x-sidecar-upstream-headers-b64: {encoded}")

decoded = base64.b64decode(encoded)
headers = json.loads(decoded)
print(f"解码后: {headers}")

if "Content-Type" in headers:
    ct = headers["Content-Type"]
    if isinstance(ct, list) and len(ct) > 0:
        print(f"✓ Content-Type = {ct[0]}")
        if "text/event-stream" in ct[0]:
            print("✓ 是流式响应，应该进入 streamSSE 路径")
    else:
        print(f"✗ Content-Type 格式异常: {ct}")
