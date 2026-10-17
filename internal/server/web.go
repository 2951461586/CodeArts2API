package server

import (
	_ "embed"
	"net/http"
)

// 控制台由三个内嵌文件组成：单页 HTML + 一个样式表 + 一个脚本。
//
// 拆开（而不是全部内联）是为了让 CSP 能收紧到 script-src 'self' /
// style-src 'self'：内联 <script> 与内联 style 属性会迫使 CSP 放开
// 'unsafe-inline'，等于放弃对注入点的最后一道防线。
// 三个文件仍由本程序内嵌提供，不依赖任何外部 CDN，可离线使用。
var (
	//go:embed web/index.html
	consoleHTML []byte
	//go:embed web/console.css
	consoleCSS []byte
	//go:embed web/console.js
	consoleJS []byte
)

// consoleCSP 是控制台页面与静态资源共用的 CSP。
//
// default-src 'none' 阻止一切外部加载；脚本与样式只允许同源；
// connect-src 'self' 限定 fetch 目标；form-action / base-uri /
// frame-ancestors 分别阻断表单外发、<base> 劫持与点击劫持 ——
// 即使将来引入注入点，数据也运不出去、脚本也执行不了。
const consoleCSP = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'none'; " +
	"connect-src 'self'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"

// consoleHeaders 统一下发控制台资源的安全响应头。
func consoleHeaders(h http.Header, contentType string) {
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", consoleCSP)
}

// serveConsole 输出控制台单页。
func serveConsole(w http.ResponseWriter, _ *http.Request) {
	consoleHeaders(w.Header(), "text/html; charset=utf-8")
	_, _ = w.Write(consoleHTML)
}

// serveConsoleCSS 输出控制台样式表。
func serveConsoleCSS(w http.ResponseWriter, _ *http.Request) {
	consoleHeaders(w.Header(), "text/css; charset=utf-8")
	_, _ = w.Write(consoleCSS)
}

// serveConsoleJS 输出控制台脚本。
func serveConsoleJS(w http.ResponseWriter, _ *http.Request) {
	consoleHeaders(w.Header(), "application/javascript; charset=utf-8")
	_, _ = w.Write(consoleJS)
}
