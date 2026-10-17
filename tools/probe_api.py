#!/usr/bin/env python3
"""CodeArts Agent 上游 API 连通性 PoC。

复刻 huaweicloud-sdk-core 的 SDK-HMAC-SHA256 签名，直连：
    POST https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions
    GET  https://opengw.developer.huaweicloud.com/api/v1/gateway/config   (可用模型)
    GET  https://opengw.developer.huaweicloud.com/api/v1/user/tokens/balance (额度)

签名细节（与 vscode-codebot/out/extension.js 里打包的 signer 逐字对齐）：
  * CanonicalURI 每个 path segment 单独 urlencode，且**强制补尾斜杠** `/a/b` -> `/a/b/`
  * CanonicalHeaders 每个 `k:v.trim()` 用 \n 连接后**再补一个 \n**，之后才是 SignedHeaders
  * 所有请求头（含 x-security-token）都参与签名
  * 请求头必须含 host 与 x-sdk-date

用法:
    python tools/probe_api.py                    # 列模型 + 一次非流式对话
    python tools/probe_api.py --stream           # 流式对话
    python tools/probe_api.py --tools            # 工具调用
    python tools/probe_api.py --model glm-5.3-flash --prompt "你好"
"""
import argparse
import datetime
import hashlib
import hmac
import http.client
import json
import sys
import urllib.error
import urllib.parse
import urllib.request
import uuid

sys.path.insert(0, __import__("os").path.dirname(__file__))
from decrypt_session import load_session  # noqa: E402

CHAT_HOST = "https://snap-access.cn-north-4.myhuaweicloud.com"
CHAT_PATH = "/api/v2/chat/completions"
BENEFIT_HOST = "https://opengw.developer.huaweicloud.com"
GATEWAY_CONFIG = "/api/v1/gateway/config"
TOKENS_BALANCE = "/api/v1/user/tokens/balance"


def sign(ak, sk, sts, method, url, body=b"", headers=None):
    u = urllib.parse.urlsplit(url)
    hdrs = {
        "host": u.netloc,
        "x-sdk-date": datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ"),
    }
    if headers:
        hdrs.update({k.lower(): v for k, v in headers.items()})
    if sts:
        hdrs["x-security-token"] = sts

    query = "&".join(sorted(u.query.split("&"))) if u.query else ""
    segs = (u.path or "/").split("/")
    curi = "/".join(urllib.parse.quote(s, safe="~") for s in segs)
    if not curi.endswith("/"):
        curi += "/"

    signed_headers = ";".join(sorted(hdrs))
    canonical_headers = "".join(f"{k}:{hdrs[k].strip()}\n" for k in sorted(hdrs))
    payload_hash = hashlib.sha256(body).hexdigest()
    canonical_request = "\n".join(
        [method, curi, query, canonical_headers, signed_headers, payload_hash]
    )
    string_to_sign = "\n".join([
        "SDK-HMAC-SHA256",
        hdrs["x-sdk-date"],
        hashlib.sha256(canonical_request.encode()).hexdigest(),
    ])
    sig = hmac.new(sk.encode(), string_to_sign.encode(), hashlib.sha256).hexdigest()
    hdrs["Authorization"] = (
        f"SDK-HMAC-SHA256 Access={ak}, SignedHeaders={signed_headers}, Signature={sig}"
    )
    return hdrs


def request(
    sess, method, url, obj=None, extra=None, stream=False
) -> tuple[int, str | http.client.HTTPResponse]:
    """发一次已签名的请求。

    stream=False 时返回 (状态码, 文本)；stream=True 时返回 (状态码, HTTPResponse)，
    由调用方逐行读取（见 _read_text / --stream 分支）。
    """
    body = json.dumps(obj).encode() if obj is not None else b""
    headers = {"content-type": "application/json", "app-id": "CodeAgent3.0"}
    if extra:
        headers.update(extra)
    hdrs = sign(sess["accessKey"], sess["secretKey"], sess["securitytoken"], method, url, body, headers)
    req = urllib.request.Request(url, data=body if method != "GET" else None, method=method, headers=hdrs)  # noqa: S310 - url 只来自本文件的常量
    try:
        resp = urllib.request.urlopen(req, timeout=300)  # noqa: S310 - 同上
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode("utf-8", "replace")
    if stream:
        return resp.status, resp
    with resp:
        return resp.status, resp.read().decode("utf-8", "replace")


def _read_text(res: str | http.client.HTTPResponse) -> str:
    """把 request() 的返回统一成文本（stream=True 时拿到的是 HTTPResponse）。"""
    if isinstance(res, str):
        return res
    with res:
        return res.read().decode("utf-8", "replace")


def chat_headers(model):
    return {
        "maas_type": "benefit",
        "X-Language": "zh-cn",
        "model-id": model,
        "model-name": model,
        "x-ot-function": "agent-general",
        "x-ot-trace-id": uuid.uuid4().hex,
        "x-snap-traceid": uuid.uuid4().hex,
        "created-time": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "x-ot-client-type": "IDE",
        "x-ot-client-version": "26.9.902",
        "plugin-name": "codemate_vscode",
        "plugin-version": "26.9.902",
        "ide-name": "VSCode",
        "ide-version": "26.9.902",
    }


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--user-data", default=None)
    ap.add_argument("--model", default="glm-5.3-flash")
    ap.add_argument("--prompt", default="只回复两个字：你好")
    ap.add_argument("--stream", action="store_true")
    ap.add_argument("--tools", action="store_true")
    args = ap.parse_args()

    from decrypt_session import default_user_data
    sess = load_session(args.user_data or default_user_data())
    print("账号:", sess.get("account", {}).get("label"), "| 过期:", sess.get("expires_at"))

    code, txt = request(sess, "GET", BENEFIT_HOST + GATEWAY_CONFIG)
    print("\n== 可用模型 ==", code)
    print(_read_text(txt)[:800])

    code, txt = request(sess, "GET", BENEFIT_HOST + TOKENS_BALANCE)
    print("\n== 额度 ==", code)
    print(_read_text(txt)[:400])

    body = {
        "model": args.model,
        "messages": [{"role": "user", "content": args.prompt}],
        "stream": bool(args.stream),
        "max_tokens": 256,
    }
    if args.tools:
        body["tools"] = [{
            "type": "function",
            "function": {
                "name": "get_weather",
                "description": "查询城市天气",
                "parameters": {
                    "type": "object",
                    "properties": {"city": {"type": "string"}},
                    "required": ["city"],
                },
            },
        }]
        body["tool_choice"] = "auto"

    code, res = request(sess, "POST", CHAT_HOST + CHAT_PATH, body, chat_headers(args.model), stream=args.stream)
    print("\n== 对话 ==", code)
    if args.stream:
        if isinstance(res, str):
            print(res[:1500])
            return 0
        for i, line in enumerate(res):
            print(line.decode("utf-8", "replace").rstrip())
            if i > 20:
                break
    else:
        print(_read_text(res)[:1500])
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
