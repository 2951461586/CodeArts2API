#!/usr/bin/env python3
"""本机 session → 2API 能力端到端测试。

覆盖：非流式对话 / 流式对话 / 思考(reasoning_content) / 工具调用(单轮+多轮闭环)
      / Anthropic Messages(text + thinking + tool_use) / 错误路径。

只用 benefit 通道模型（免费额度），不触碰积分模型。
"""
import json
import sys
import time
import urllib.error
import urllib.request

BASE = "http://127.0.0.1:10087"
BENEFIT_MODEL = "glm-5.3-flash"

results = []


def post(path, payload, headers=None, timeout=180, raw=False):
    data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(BASE + path, data=data, method="POST")
    req.add_header("Content-Type", "application/json; charset=utf-8")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            body = r.read()
            return r.status, body, time.time() - t0
    except urllib.error.HTTPError as e:
        return e.code, e.read(), time.time() - t0


def get(path, timeout=60):
    with urllib.request.urlopen(BASE + path, timeout=timeout) as r:
        return r.status, r.read()


def post_stream(path, payload, headers=None, timeout=180):
    """返回 (status, [raw sse lines], elapsed)."""
    data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(BASE + path, data=data, method="POST")
    req.add_header("Content-Type", "application/json; charset=utf-8")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    t0 = time.time()
    lines = []
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            status = r.status
            for raw in r:
                lines.append(raw.decode("utf-8", "replace").rstrip("\n"))
            return status, lines, time.time() - t0
    except urllib.error.HTTPError as e:
        return e.code, [e.read().decode("utf-8", "replace")], time.time() - t0


def check(name, ok, detail=""):
    results.append((name, ok, detail))
    print(("[PASS] " if ok else "[FAIL] ") + name + ((" :: " + detail) if detail else ""))
    sys.stdout.flush()


# ---------------------------------------------------------------- 1. 非流式
def t_blocking():
    st, body, el = post("/v1/chat/completions", {
        "model": BENEFIT_MODEL,
        "messages": [{"role": "user", "content": "只回答两个字：你好"}],
        "max_tokens": 512,
        "stream": False,
    })
    try:
        d = json.loads(body)
    except Exception:
        return check("非流式对话", False, body[:200].decode("utf-8", "replace"))
    msg = (d.get("choices") or [{}])[0].get("message", {})
    content = msg.get("content") or ""
    think = msg.get("reasoning_content") or ""
    check("非流式对话", st == 200 and bool(content or think),
          f"status={st} {el:.1f}s content={content[:60]!r} think_len={len(think)} finish={d.get('choices',[{}])[0].get('finish_reason')}")
    return content, think


# ---------------------------------------------------------------- 2. 流式
def t_streaming():
    st, lines, el = post_stream("/v1/chat/completions", {
        "model": BENEFIT_MODEL,
        "messages": [{"role": "user", "content": "从1数到5，只输出数字"}],
        "max_tokens": 1024,
        "stream": True,
    })
    chunks = [l for l in lines if l.startswith("data: ")]
    done = any(l.strip() == "data: [DONE]" for l in lines)
    text, think, usage = "", "", None
    for l in chunks:
        p = l[6:].strip()
        if p == "[DONE]":
            continue
        try:
            o = json.loads(p)
        except Exception:
            continue
        if o.get("usage"):
            usage = o["usage"]
        for c in o.get("choices") or []:
            dl = c.get("delta") or {}
            text += dl.get("content") or ""
            think += dl.get("reasoning_content") or ""
    check("流式对话", st == 200 and done and bool(text or think),
          f"status={st} chunks={len(chunks)} {el:.1f}s text={text[:60]!r} think_len={len(think)} done={done} usage={usage}")
    return text, think


# ---------------------------------------------------------------- 3. 工具调用（单轮）
TOOLS = [{
    "type": "function",
    "function": {
        "name": "get_weather",
        "description": "查询指定城市的实时天气",
        "parameters": {
            "type": "object",
            "properties": {
                "city": {"type": "string", "description": "城市名，例如 北京"},
                "unit": {"type": "string", "enum": ["celsius", "fahrenheit"]},
            },
            "required": ["city"],
        },
    },
}]


def t_tool_call_blocking():
    st, body, el = post("/v1/chat/completions", {
        "model": BENEFIT_MODEL,
        "messages": [{"role": "user", "content": "帮我查一下北京现在的天气，用工具。"}],
        "tools": TOOLS,
        "tool_choice": "auto",
        "max_tokens": 1024,
        "stream": False,
    })
    try:
        d = json.loads(body)
    except Exception:
        return check("工具调用(非流式)", False, body[:200].decode("utf-8", "replace"))
    ch = (d.get("choices") or [{}])[0]
    msg = ch.get("message", {})
    calls = msg.get("tool_calls") or []
    ok = st == 200 and len(calls) > 0
    detail = f"status={st} {el:.1f}s finish={ch.get('finish_reason')} calls="
    detail += json.dumps([{"name": c["function"]["name"], "args": c["function"]["arguments"]} for c in calls], ensure_ascii=False)
    if not ok:
        detail += " content=" + repr((msg.get("content") or "")[:120])
    check("工具调用(非流式)", ok, detail)
    return calls


# ---------------------------------------------------------------- 4. 工具调用（流式）
def t_tool_call_stream():
    st, lines, el = post_stream("/v1/chat/completions", {
        "model": BENEFIT_MODEL,
        "messages": [{"role": "user", "content": "上海天气怎么样？请调用工具查询。"}],
        "tools": TOOLS,
        "max_tokens": 1024,
        "stream": True,
    })
    names, args, finish = {}, {}, None
    for l in lines:
        if not l.startswith("data: "):
            continue
        p = l[6:].strip()
        if p == "[DONE]":
            continue
        try:
            o = json.loads(p)
        except Exception:
            continue
        for c in o.get("choices") or []:
            if c.get("finish_reason"):
                finish = c["finish_reason"]
            for tc in (c.get("delta") or {}).get("tool_calls") or []:
                i = tc.get("index", 0)
                if tc.get("function", {}).get("name"):
                    names[i] = tc["function"]["name"]
                args[i] = args.get(i, "") + (tc.get("function", {}).get("arguments") or "")
    ok = st == 200 and bool(names)
    check("工具调用(流式)", ok, f"status={st} {el:.1f}s finish={finish} names={names} args={args}")


# ---------------------------------------------------------------- 5. 多轮工具闭环
def t_tool_roundtrip(calls):
    if not calls:
        return check("工具调用多轮闭环", False, "前置单轮工具调用未拿到 tool_calls")
    call = calls[0]
    msgs = [
        {"role": "user", "content": "帮我查一下北京现在的天气，用工具。"},
        {"role": "assistant", "content": None, "tool_calls": [call]},
        {"role": "tool", "tool_call_id": call.get("id"), "content": json.dumps({"city": "北京", "temp_c": 21, "desc": "晴"}, ensure_ascii=False)},
    ]
    st, body, el = post("/v1/chat/completions", {
        "model": BENEFIT_MODEL, "messages": msgs, "tools": TOOLS,
        "max_tokens": 1024, "stream": False,
    })
    try:
        d = json.loads(body)
    except Exception:
        return check("工具调用多轮闭环", False, body[:200].decode("utf-8", "replace"))
    msg = (d.get("choices") or [{}])[0].get("message", {})
    text = msg.get("content") or ""
    ok = st == 200 and bool(text.strip())
    check("工具调用多轮闭环", ok, f"status={st} {el:.1f}s content={text[:120]!r}")


# ---------------------------------------------------------------- 6. Anthropic /v1/messages
def t_anthropic():
    st, body, el = post("/v1/messages", {
        "model": BENEFIT_MODEL,
        "max_tokens": 512,
        "messages": [{"role": "user", "content": "只回答两个字：你好"}],
    }, headers={"anthropic-version": "2023-06-01"})
    try:
        d = json.loads(body)
    except Exception:
        return check("Anthropic 非流式", False, body[:300].decode("utf-8", "replace"))
    blocks = d.get("content") or []
    kinds = [b.get("type") for b in blocks]
    txt = "".join(b.get("text", "") for b in blocks if b.get("type") == "text")
    think = "".join(b.get("thinking", "") for b in blocks if b.get("type") == "thinking")
    check("Anthropic 非流式", st == 200 and d.get("type") == "message" and bool(kinds),
          f"status={st} {el:.1f}s blocks={kinds} text={txt[:60]!r} think_len={len(think)} stop={d.get('stop_reason')} usage={d.get('usage')}")


def t_anthropic_stream():
    st, lines, el = post_stream("/v1/messages", {
        "model": BENEFIT_MODEL,
        "max_tokens": 512,
        "messages": [{"role": "user", "content": "只回答两个字：你好"}],
        "stream": True,
    }, headers={"anthropic-version": "2023-06-01"})
    events = [l[7:] for l in lines if l.startswith("event: ")]
    text = ""
    for l in lines:
        if l.startswith("data: "):
            try:
                o = json.loads(l[6:])
            except Exception:
                continue
            if o.get("type") == "content_block_delta":
                text += (o.get("delta") or {}).get("text", "") or (o.get("delta") or {}).get("thinking", "")
    ok = st == 200 and "message_start" in events and "message_stop" in events
    check("Anthropic 流式", ok, f"status={st} {el:.1f}s events={sorted(set(events))} text={text[:60]!r}")


def t_anthropic_tool():
    st, body, el = post("/v1/messages", {
        "model": BENEFIT_MODEL,
        "max_tokens": 1024,
        "messages": [{"role": "user", "content": "北京天气如何？请用工具查询。"}],
        "tools": [{"name": "get_weather", "description": "查询城市天气",
                   "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}],
    }, headers={"anthropic-version": "2023-06-01"})
    try:
        d = json.loads(body)
    except Exception:
        return check("Anthropic 工具调用", False, body[:300].decode("utf-8", "replace"))
    blocks = d.get("content") or []
    uses = [b for b in blocks if b.get("type") == "tool_use"]
    ok = st == 200 and bool(uses)
    check("Anthropic 工具调用", ok,
          f"status={st} {el:.1f}s stop={d.get('stop_reason')} tool_use=" +
          json.dumps([{"name": u.get("name"), "input": u.get("input")} for u in uses], ensure_ascii=False))


# ---------------------------------------------------------------- 7. 错误路径
def t_errors():
    # 空 messages
    st, body, _ = post("/v1/chat/completions", {"model": BENEFIT_MODEL, "messages": []})
    check("错误路径: 空 messages 返回 400", st == 400, f"status={st} body={body[:120].decode('utf-8','replace')}")
    # 未知模型应回落到默认（不应 500）
    st, body, _ = post("/v1/chat/completions", {
        "model": "no-such-model-xyz", "messages": [{"role": "user", "content": "hi"}],
        "max_tokens": 64, "stream": False})
    check("错误路径: 未知模型回落而非 500", st in (200, 502), f"status={st}")
    # 坏 JSON
    req = urllib.request.Request(BASE + "/v1/chat/completions", data=b"{not json", method="POST")
    req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            st = r.status
    except urllib.error.HTTPError as e:
        st = e.code
    check("错误路径: 坏 JSON 返回 400", st == 400, f"status={st}")
    # 404
    try:
        get("/no/such/path")
        st = 200
    except urllib.error.HTTPError as e:
        st = e.code
    check("错误路径: 未知路径 404", st == 404, f"status={st}")


def main():
    print("== 本机 session → 2API 能力测试 ==")
    st, body = get("/health")
    print("health:", body.decode())
    t_blocking()
    t_streaming()
    calls = t_tool_call_blocking()
    t_tool_call_stream()
    t_tool_roundtrip(calls)
    t_anthropic()
    t_anthropic_stream()
    t_anthropic_tool()
    t_errors()
    passed = sum(1 for _, ok, _ in results if ok)
    print(f"\n== 汇总: {passed}/{len(results)} 通过 ==")
    for n, ok, d in results:
        if not ok:
            print("  FAIL:", n, "::", d)
    return 0 if passed == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
