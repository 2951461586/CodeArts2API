package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// 控制台脚本只有一个文件 —— 任何一处语法错误都会让**整段脚本**不执行：
// 按钮、事件绑定、初始数据加载全部失效，用户看到的就是
// 「能输密码但点什么都没反应」。
//
// 最容易踩的坑是把真实换行打进字符串字面量里（JS 的 "..." 不能跨行），
// 以及把 `on("#x", function(){...});` 写成 `on("#x", function(){...};`
// （括号不配对）。下面两个测试在 go test 阶段就把它们挡住。

const (
	jsBackslash = 0x5c // \
	jsQuote     = 0x22 // "
	jsSQuote    = 0x27 // '
	jsBacktick  = 0x60 // `
)

// TestConsoleScriptHasNoMultilineString 找出「双引号数量为奇数」的行 ——
// 即字符串跨行。
func TestConsoleScriptHasNoMultilineString(t *testing.T) {
	lines := strings.Split(string(consoleJS), "\n")
	var bad []int
	for i, line := range lines {
		if countDoubleQuotes(stripLiteralsAndComments(line))%2 == 1 {
			bad = append(bad, i+1)
		}
	}
	if len(bad) > 0 {
		t.Errorf("第 %v 行有跨行的双引号字符串 —— 整段脚本会 SyntaxError，"+
			"页面上所有按钮与事件绑定都会失效", bad)
		for _, ln := range bad {
			t.Logf("line %d: %s", ln, lines[ln-1])
		}
	}
}

// TestConsoleScriptBalanced 校验括号 / 花括号 / 方括号配对。
func TestConsoleScriptBalanced(t *testing.T) {
	body := string(consoleJS)
	stack := []byte{}
	pairs := map[byte]byte{'}': '{', ')': '(', ']': '['}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch c {
		case jsQuote, jsSQuote, jsBacktick:
			i = skipString(body, i)
		case '/':
			if i+1 < len(body) && body[i+1] == '/' {
				for i < len(body) && body[i] != '\n' {
					i++
				}
			}
		case '{', '(', '[':
			stack = append(stack, c)
		case '}', ')', ']':
			if len(stack) == 0 {
				t.Fatalf("偏移 %d 处多了一个 %q", i, c)
			}
			if stack[len(stack)-1] != pairs[c] {
				t.Fatalf("偏移 %d 处括号不匹配：期望闭合 %q，实际 %q",
					i, string(stack[len(stack)-1]), string(c))
			}
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) > 0 {
		t.Errorf("有 %d 个未闭合的括号: %q", len(stack), string(stack))
	}
}

// skipString 返回字符串字面量结束后的下标（跳过转义）。
func skipString(s string, i int) int {
	q := s[i]
	i++
	for i < len(s) {
		if s[i] == jsBackslash {
			i += 2
			continue
		}
		if s[i] == q {
			return i
		}
		i++
	}
	return i
}

// stripLiteralsAndComments 剥掉单引号串、模板串与行注释，只留双引号串和代码骨架。
func stripLiteralsAndComments(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case jsSQuote, jsBacktick:
			i = skipString(s, i)
		case '/':
			if i+1 < len(s) && s[i+1] == '/' {
				return b.String()
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// countDoubleQuotes 数一行里的双引号个数（不含被剥掉的部分）。
func countDoubleQuotes(s string) int {
	return strings.Count(s, string(rune(jsQuote)))
}

// TestConsoleSendsStrictCSP 控制台三类资源的 CSP 都必须收紧：
// 不允许 'unsafe-inline'，脚本与样式只允许同源。
func TestConsoleSendsStrictCSP(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		path string
	}{
		{"html", serveConsole, "/"},
		{"css", serveConsoleCSS, "/console.css"},
		{"js", serveConsoleJS, "/console.js"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.h(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			csp := rec.Header().Get("Content-Security-Policy")
			if csp == "" {
				t.Fatal("响应缺少 Content-Security-Policy")
			}
			for _, want := range []string{
				"default-src 'none'",
				"script-src 'self'",
				"style-src 'self'",
				"connect-src 'self'",
				"base-uri 'none'",
				"form-action 'none'",
				"frame-ancestors 'none'",
			} {
				if !strings.Contains(csp, want) {
					t.Errorf("CSP 缺少 %q：%s", want, csp)
				}
			}
			// 收紧的核心：不允许内联执行。
			if strings.Contains(csp, "unsafe-inline") {
				t.Errorf("CSP 仍包含 'unsafe-inline'：%s", csp)
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Error("缺少 X-Content-Type-Options: nosniff")
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Error("缺少 Cache-Control: no-store")
			}
		})
	}
}

// TestConsoleHTMLHasNoInlineCode 页面本身不得再有内联 <style> / <script> 内容，
// 也不得有内联 style 属性或内联事件处理器 —— 否则上面的严格 CSP 会直接
// 让页面样式与交互失效（这正是收紧 CSP 的前置条件）。
func TestConsoleHTMLHasNoInlineCode(t *testing.T) {
	html := string(consoleHTML)

	if strings.Contains(html, "<style") {
		t.Error("index.html 仍包含内联 <style>，CSP style-src 'self' 会拒绝它")
	}
	// RE2 不支持负向断言，改为逐个检查 <script ...> 标签是否都带 src。
	for _, tag := range regexp.MustCompile(`(?s)<script[^>]*>`).FindAllString(html, -1) {
		if !strings.Contains(tag, "src=") {
			t.Errorf("index.html 仍包含内联 <script>（没有 src），CSP script-src 'self' 会拒绝它: %s", tag)
		}
	}
	if strings.Contains(html, `style="`) {
		t.Error("index.html 仍包含内联 style 属性，CSP style-src 'self' 会拒绝它（改用工具类）")
	}
	for _, attr := range []string{" onclick=", " onload=", " onerror=", " onmouseover=", "javascript:"} {
		if strings.Contains(html, attr) {
			t.Errorf("控制台存在内联事件处理器 %q，与收紧后的 CSP 冲突", attr)
		}
	}
	// 必须引用同源的样式表与脚本。
	for _, want := range []string{`href="/console.css"`, `src="/console.js"`} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html 缺少对 %s 的引用", want)
		}
	}
}

// TestConsoleUsesTextContentOnly 回归：脚本不得使用 HTML 拼接 API（XSS 面）。
func TestConsoleUsesTextContentOnly(t *testing.T) {
	js := string(consoleJS)
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function("} {
		if strings.Contains(js, sink) {
			t.Errorf("控制台使用了危险的 HTML/JS 拼接 API: %s", sink)
		}
	}
}

// TestConsoleDOMContract 校验脚本里引用的每个元素 id 都真实存在于 HTML。
//
// 纯静态检查能挡住「改了 id 忘了改 JS」这类只在浏览器里才暴露的错误；
// 注意 `"#tab-" + name` 这种动态拼接不在覆盖范围内，因此页签相关 id
// 必须显式出现在 TAB_IDS 映射里（见 app.js）。
func TestConsoleDOMContract(t *testing.T) {
	html := string(consoleHTML)
	js := string(consoleJS)

	ids := map[string]bool{}
	for _, m := range regexp.MustCompile(`\bid="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		if ids[m[1]] {
			t.Errorf("index.html 存在重复 id: %q", m[1])
		}
		ids[m[1]] = true
	}

	refRe := regexp.MustCompile(`(?:\$|on)\("#([^"]+)"`)
	for _, m := range refRe.FindAllStringSubmatch(js, -1) {
		if !ids[m[1]] {
			t.Errorf("脚本引用了不存在的元素 id: %q", m[1])
		}
	}
}
