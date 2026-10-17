#!/usr/bin/env python3
"""校验内嵌控制台的 JS / CSS 是否可解析。

背景：控制台脚本是**单个文件**，任何一处语法错误都会让整段脚本不执行 ——
按钮、事件绑定、初始数据加载全部失效，表现就是「能输密码但点什么都没反应」。
最容易踩的坑有两个：

  1. 把真实换行打进了双引号字符串里（JS 的 "..." 不能跨行）；
  2. 把 `on("#x", function(){...});` 写成 `on("#x", function(){...};`（括号不配对）。

JS 做两层检查：
  * 轻量（总是执行）—— 找「双引号数量为奇数」的行，即跨行字符串；
  * 彻底（有 node 时）—— 交给 `node --check`，任何语法错误都能抓到。

CSS 做两项检查：花括号配对，以及不得引用外部资源
（`url(...)` / `@import`），否则收紧后的 CSP 会静默拦截它们。

用法:
    python tools/check_html_js.py                    # 默认检查 app.js + app.css
    python tools/check_html_js.py path/to/app.js
    python tools/check_html_js.py path/to/index.html  # 会顺着 <script src> 找到 JS
"""
import os
import re
import shutil
import subprocess
import sys
import tempfile

BS = chr(92)  # backslash
QUOTE = chr(34)  # double quote

WEB_DIR = os.path.join("internal", "server", "web")


def strip_literals_and_comments(s: str) -> str:
    """剥掉单引号串、模板串、行注释，只留双引号串与代码骨架。"""
    out = []
    i, n = 0, len(s)
    while i < n:
        c = s[i]
        if c == "'":
            i += 1
            while i < n and s[i] != "'":
                i += 2 if s[i] == BS else 1
            i += 1
        elif c == "`":
            i += 1
            while i < n and s[i] != "`":
                i += 2 if s[i] == BS else 1
            i += 1
        elif c == "/" and i + 1 < n and s[i + 1] == "/":
            break
        else:
            out.append(c)
            i += 1
    return "".join(out)


def read(path: str):
    """读文件；失败返回 None（错误已打印）。"""
    try:
        with open(path, encoding="utf-8") as fh:
            return fh.read()
    except OSError as exc:
        print(f"无法读取 {path}: {exc}")
        return None


def resolve_js(target: str):
    """把目标解析成 (js 路径, js 源码)。支持 .js 与引用外链脚本的 .html。"""
    if target.endswith(".js"):
        src = read(target)
        return (target, src) if src is not None else (target, None)

    text = read(target)
    if text is None:
        return target, None

    m = re.search(r'<script[^>]*\bsrc="([^"]+)"', text)
    if m:
        # src 是 URL 路径（/console.js），映射到 HTML 同目录下的文件名
        rel = m.group(1).lstrip("/")
        js_path = os.path.join(os.path.dirname(target), os.path.basename(rel))
        return js_path, read(js_path)

    # 兼容仍是内联 <script> 的旧布局
    m = re.search(r"<script>(.*?)</script>", text, re.S)
    if m:
        print(f"{target}: 警告——仍是内联 <script>，建议拆成独立文件以便收紧 CSP")
        return target, m.group(1)

    print(f"{target}: 没有找到 <script src> 或内联 <script>")
    return target, None


def check_multiline_strings(script: str) -> list:
    bad = []
    for i, line in enumerate(script.split("\n")):
        if strip_literals_and_comments(line).count(QUOTE) % 2 == 1:
            bad.append(i + 1)
    return bad


def check_with_node(script: str):
    node = shutil.which("node")
    if not node:
        return None, "未找到 node，跳过彻底校验"
    with tempfile.NamedTemporaryFile(
        "w", suffix=".js", encoding="utf-8", delete=False
    ) as tmp:
        tmp.write(script)
        path = tmp.name
    try:
        proc = subprocess.run(
            [node, "--check", path], capture_output=True, text=True, timeout=60
        )
    finally:
        os.unlink(path)
    return proc.returncode == 0, proc.stderr.strip()


def check_css(path: str) -> int:
    """检查 CSS：花括号配对 + 不引用外部资源。"""
    text = read(path)
    if text is None:
        return 2
    # 去掉注释再数括号
    body = re.sub(r"/\*.*?\*/", "", text, flags=re.S)
    if body.count("{") != body.count("}"):
        print(f"{path}: 花括号不配对（{body.count('{')} 开 / {body.count('}')} 闭）")
        return 1
    ext = re.findall(r"@import|url\(\s*['\"]?(https?:)?//", body)
    if ext:
        print(f"{path}: 引用了外部资源 {ext}，收紧后的 CSP 会拦截它")
        return 1
    print(f"{path}: OK（{len(text)} 字节，花括号配对，无外部资源）")
    return 0


def main() -> int:
    target = sys.argv[1] if len(sys.argv) > 1 else os.path.join(WEB_DIR, "console.js")
    js_path, script = resolve_js(target)
    if script is None:
        return 1

    bad = check_multiline_strings(script)
    if bad:
        print(f"{js_path}: 发现 {len(bad)} 处跨行的双引号字符串 → 整段脚本会 SyntaxError")
        print()
        lines = script.split("\n")
        for ln in bad:
            print(f"--- line {ln} ---")
            for k in range(max(0, ln - 3), min(len(lines), ln + 2)):
                print(f"{k+1:5d}| {lines[k]}")
            print()
        return 1

    ok, detail = check_with_node(script)
    if ok is None:
        print(f"{js_path}: 跨行字符串检查 OK；{detail}")
    elif not ok:
        print(f"{js_path}: node --check 报语法错误 → 整个脚本不会执行")
        print(detail)
        return 1
    else:
        print(f"{js_path}: OK（{len(script)} 字节，node --check 通过）")

    # 只要目标是默认布局（或 HTML），顺带把 CSS 也查了。
    if target.endswith(".html") or os.path.basename(target) == "console.js":
        css = os.path.join(os.path.dirname(js_path), "console.css")
        if os.path.exists(css):
            return check_css(css)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
