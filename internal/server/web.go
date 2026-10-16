package server

import (
	_ "embed"
	"net/http"
)

// consoleHTML 是内嵌的控制台单页（单文件、无外部依赖、可离线使用）。
//
//go:embed web/index.html
var consoleHTML []byte

// serveConsole 输出控制台单页。
//
// 响应头刻意收紧：禁止缓存、禁止 MIME 嗅探、不发送 Referer，并下发 CSP。
//
// CSP 说明：页面是一个内联 <style> + 一个内联 <script>，所以 script-src /
// style-src 必须放开 'unsafe-inline'（页面里已无任何内联事件处理器，
// 也不引用任何外部资源）。其余全部锁死：default-src 'none' 阻止一切外部加载，
// connect-src 'self' 限定 fetch 目标，form-action / base-uri / frame-ancestors
// 分别阻断表单外发、<base> 劫持与点击劫持 —— 即使将来引入注入点，
// 数据也运不出去。
func serveConsole(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy",
		"default-src 'none'; "+
			"script-src 'unsafe-inline'; "+
			"style-src 'unsafe-inline'; "+
			"connect-src 'self'; "+
			"base-uri 'none'; "+
			"form-action 'none'; "+
			"frame-ancestors 'none'")
	_, _ = w.Write(consoleHTML)
}
