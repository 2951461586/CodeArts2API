---
id: adr-0002
type: submodule-design
title: "ADR 0002：控制台拆为三个内嵌文件，CSP 去掉 unsafe-inline"
status: active
parent: server
---

# ADR 0002：控制台拆为三个内嵌文件，CSP 去掉 `unsafe-inline`

- 状态：已采纳
- 日期：2026-10

## 背景

控制台最初是**一个 HTML 文件**，内含 `<style>` 与 `<script>`，另有 51 处内联
`style="..."` 属性。这让 CSP 只能写成：

```text
script-src 'unsafe-inline'; style-src 'unsafe-inline'
```

也就是说，CSP 对最常见的注入点（内联脚本、内联样式）**完全没有防护作用**。
而控制台能读到管理密码、能导出完整登录态（含 `refresh_token`），
是整个项目里最值得加固的面。

## 决策

1. 拆成三个文件：`web/index.html`、`web/console.css`、`web/console.js`，
   全部 `go:embed` 内嵌、同源提供（`/`、`/console.css`、`/console.js`）；
2. 51 处内联 `style` 属性收敛为工具类（`.mt-12`、`.w-full`、`.grow-320` …）；
3. CSP 收紧为：

```text
default-src 'none'; script-src 'self'; style-src 'self'; img-src 'none';
connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'
```

## 后果

### 好处

- 即使将来引入注入点，也**执行不了脚本**、**加载不了外部资源**；
- 样式与脚本可被浏览器正常缓存策略管理（当前仍 `no-store`，保持简单）；
- 控制台 JS/CSS 可被独立校验（`tools/check_html_js.py`、`go test`）。

### 代价

- README 里「单文件」的说法要改成「三个内嵌文件」；
- 新增两个路由，需保证它们也带上同样的安全响应头（已统一走 `consoleHeaders`）；
- 内联样式改成类名后，改样式必须去 CSS 文件，不能再就地改 HTML。

### 回归防护

`internal/server/console_test.go` 有四道守卫：
`TestConsoleSendsStrictCSP`（断言不含 `unsafe-inline`）、
`TestConsoleHTMLHasNoInlineCode`（无内联 style/script）、
`TestConsoleScriptBalanced` / `TestConsoleScriptHasNoMultilineString`（脚本可解析）、
`TestConsoleDOMContract`（脚本引用的 id 必须存在于 HTML）。

### 被否掉的替代方案

- **哈希白名单**（`'sha256-...'`）：能保留单文件，但**内联 `style` 属性无法用哈希覆盖**
  （需要 `unsafe-hashes`），仍要先做类名收敛，收益不如直接外置。
- **nonce**：需要每次响应动态注入，把静态文件变成模板，复杂度更高。
