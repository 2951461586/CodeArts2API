package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// handleMetrics 以 Prometheus 文本格式暴露运行指标。
//
// 指标全部由现有的 Store.Stats / 账号池 / 模型清单即时计算，不新增计数器，
// 因此不会给请求路径增加任何开销。
//
// 鉴权与管理接口一致：设了管理密码则必须携带
// （Prometheus 可在 scrape 配置里用 authorization.credentials 提供）；
// 未设密码则仅允许本机直连。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	stats := s.Store.Stats()
	accounts := s.Store.Accounts()

	ready, okCount, errCount, disabled := 0, 0, 0, 0
	for _, a := range accounts {
		switch {
		case !a.Enabled:
			disabled++
		case a.Status == "ok":
			okCount++
			ready++
		default:
			errCount++
		}
	}

	uptime := int64(0)
	if stats.StartedAt > 0 {
		uptime = int64(time.Since(time.Unix(stats.StartedAt, 0)).Seconds())
		if uptime < 0 {
			uptime = 0
		}
	}

	var b strings.Builder
	writeMetric(&b, "codearts2api_requests_total", "counter", "累计请求数", stats.TotalRequests)
	writeMetric(&b, "codearts2api_requests_failed_total", "counter", "累计失败请求数", stats.FailedRequests)
	writeMetric(&b, "codearts2api_output_chars_total", "counter", "累计输出字符数", stats.TotalCharsOut)
	writeMetric(&b, "codearts2api_accounts_ready", "gauge", "可用账号数（启用且状态 ok）", int64(ready))
	writeMetric(&b, "codearts2api_models", "gauge", "当前可用模型数", int64(len(s.Reg.List(nil))))
	writeMetric(&b, "codearts2api_uptime_seconds", "gauge", "进程运行时长（秒）", uptime)

	b.WriteString("# HELP codearts2api_accounts 账号数（按状态）\n")
	b.WriteString("# TYPE codearts2api_accounts gauge\n")
	for _, kv := range []struct {
		status string
		n      int
	}{
		{"ok", okCount},
		{"error", errCount},
		{"disabled", disabled},
	} {
		fmt.Fprintf(&b, "codearts2api_accounts{status=%q} %d\n", metricLabel(kv.status), kv.n)
	}

	h := w.Header()
	h.Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(b.String()))
}

// writeMetric 输出一个无标签的指标。
func writeMetric(b *strings.Builder, name, typ, help string, v int64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", name, help, name, typ, name, v)
}

// metricLabel 按 Prometheus 文本格式转义标签值。
func metricLabel(s string) string {
	if !strings.ContainsAny(s, `\"`+"\n") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}
