#!/usr/bin/env python3
"""校验内嵌控制台单页 index.html 里的 JS 是否可解析。

背景：页面只有一个 <script> 区块，**任何一处语法错误都会让整段脚本不执行** ——
按钮、事件绑定、初始数据加载全部失效，表现就是「能输密码但点什么都没反应」。
最容易踩的坑是把真实换行打进了字符串字面量里（JS 的 "..." 不能跨行）。

做两层检查：

  1. 轻量（总是执行）—— 扫 <script> 区间，找「双引号数量为奇数」的行，
     也就是跨行的字符串字面量；
  2. 彻底（有 node 时）—— 把 <script> 抽出来交给 `node --check`，
     任何语法错误都能抓到。

用法:
    python tools/check_html_js.py                      # 默认查内嵌控制台
    python tools/check_html_js.py <path-to.html>
"""
import os
import shutil
import subprocess
import sys
import tempfile

BS = chr(92)  # backslash
QUOTE = chr(34)  # double quote


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


def extract_script(text: str):
    start = text.find("<script>")
    end = text.rfind("</script>")
    if start < 0 or end < 0 or end <= start:
        return None
    return text[start + len("<script>") : end]


def check_multiline_strings(lines, start, end) -> list:
    bad = []
    for i in range(start + 1, end):
        if strip_literals_and_comments(lines[i]).count(QUOTE) % 2 == 1:
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


def main() -> int:
    target = sys.argv[1] if len(sys.argv) > 1 else "internal/server/web/index.html"
    text = read(target)
    if text is None:
        return 2

    script = extract_script(text)
    if script is None:
        print(f"{target}: 没有找到 <script> 区块")
        return 1

    lines = text.split("\n")
    start = next(i for i, line in enumerate(lines) if "<script>" in line)
    end = next(i for i, line in enumerate(lines) if "</script>" in line and i > start)

    bad = check_multiline_strings(lines, start, end)
    if bad:
        print(f"{target}: 发现 {len(bad)} 处跨行的双引号字符串 → 整段 <script> 会 SyntaxError")
        print()
        for ln in bad:
            print(f"--- line {ln} ---")
            for k in range(max(start, ln - 3), min(end, ln + 2)):
                print(f"{k+1:5d}| {lines[k]}")
            print()
        return 1

    ok, detail = check_with_node(script)
    if ok is None:
        print(f"{target}: 跨行字符串检查 OK（{start+1}~{end+1} 行）；{detail}")
        return 0
    if not ok:
        print(f"{target}: node --check 报语法错误 → 整个 <script> 不会执行")
        print(detail)
        return 1

    print(f"{target}: OK（<script> {start+1}~{end+1} 行，{len(script)} 字节，node --check 通过）")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
