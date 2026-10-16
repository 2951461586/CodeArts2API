#!/usr/bin/env python3
"""CodeArts Agent 凭据提取 PoC（只读，不修改任何文件）。

CodeArts Agent（VS Code fork）把登录会话存在 VS Code 的 secret storage 里：
  %APPDATA%\\codearts-agent\\User\\globalStorage\\state.vscdb  (SQLite)
    key   = secret://{"extensionId":"huaweicloud.authentication","key":"HuaweiCloudSession"}
    value = {"type":"Buffer","data":[118,49,48,...]}   # 前缀 "v10"

加密方式和 Chromium/Electron 的 OSCrypt 完全一致（和 Doubao2API 里豆包的方案同源）：
  1. Local State 的 os_crypt.encrypted_key 是 "DPAPI" + DPAPI 保护的 32 字节 AES 主密钥
  2. 每条密文 = "v10"(3) + nonce(12) + AES-256-GCM(ciphertext||tag)

用法:
    python tools/decrypt_session.py                 # 自动定位 %APPDATA%\\codearts-agent
    python tools/decrypt_session.py <userDataDir>   # 指定 user data 目录
    python tools/decrypt_session.py --json          # 只打印 AK/SK/STS（脱敏）
"""
import base64
import ctypes
import json
import os
import sqlite3
import sys
from ctypes import wintypes

from cryptography.hazmat.primitives.ciphers.aead import AESGCM

SECRET_KEY = 'secret://{"extensionId":"huaweicloud.authentication","key":"HuaweiCloudSession"}'


class DATA_BLOB(ctypes.Structure):
    _fields_ = [("cbData", wintypes.DWORD), ("pbData", ctypes.POINTER(ctypes.c_char))]


crypt32 = ctypes.windll.crypt32
kernel32 = ctypes.windll.kernel32


def dpapi_unprotect(data: bytes) -> bytes:
    buf = ctypes.create_string_buffer(data, len(data))
    blob_in = DATA_BLOB(len(data), ctypes.cast(buf, ctypes.POINTER(ctypes.c_char)))
    blob_out = DATA_BLOB()
    if not crypt32.CryptUnprotectData(
        ctypes.byref(blob_in), None, None, None, None, 0, ctypes.byref(blob_out)
    ):
        raise OSError("CryptUnprotectData failed: %d" % ctypes.GetLastError())
    try:
        return ctypes.string_at(blob_out.pbData, blob_out.cbData)
    finally:
        kernel32.LocalFree(blob_out.pbData)


def master_key(user_data: str) -> bytes:
    with open(os.path.join(user_data, "Local State"), encoding="utf-8") as fh:
        ls = json.load(fh)
    blob = base64.b64decode(ls["os_crypt"]["encrypted_key"])
    if blob[:5] != b"DPAPI":
        raise ValueError("encrypted_key 前缀不是 DPAPI")
    return dpapi_unprotect(blob[5:])


def aes_gcm_decrypt(key: bytes, raw: bytes) -> bytes:
    if raw[:3] != b"v10":
        raise ValueError("密文前缀不是 v10，可能是旧版 safeStorage（纯 DPAPI）")
    return AESGCM(key).decrypt(raw[3:15], raw[15:], None)


def load_session(user_data: str) -> dict:
    key = master_key(user_data)
    db = os.path.join(user_data, "User", "globalStorage", "state.vscdb")
    # 只读打开，允许 IDE 同时运行
    con = sqlite3.connect("file:%s?mode=ro" % db.replace("?", "%3f"), uri=True)
    try:
        row = con.execute(
            "select value from ItemTable where key = ?", (SECRET_KEY,)
        ).fetchone()
    finally:
        con.close()
    if not row:
        raise SystemExit("未找到 HuaweiCloudSession，请先在 CodeArts Agent 里登录")
    raw = bytes(json.loads(row[0])["data"])
    return json.loads(aes_gcm_decrypt(key, raw))


def default_user_data() -> str:
    appdata = os.environ.get("APPDATA") or os.path.expanduser("~")
    return os.path.join(appdata, "codearts-agent")


def main() -> int:
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    user_data = args[0] if args else default_user_data()
    sess = load_session(user_data)
    if "--json" in sys.argv:
        print(json.dumps({
            "account": sess.get("account", {}).get("label"),
            "domainId": sess.get("domainId"),
            "accessKeyId": sess.get("accessKey"),
            "secretAccessKey": "***" if sess.get("secretKey") else "",
            "hasSecurityToken": bool(sess.get("securitytoken")),
            "expiresAt": sess.get("expires_at"),
            "hasRefreshToken": bool(sess.get("refresh_token")),
        }, ensure_ascii=False, indent=2))
    else:
        print(json.dumps(sess, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
