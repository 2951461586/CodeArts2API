package server

import (
	"strings"
	"testing"
)

// 控制台单页只有一个 <script> 区块 —— 任何一处语法错误都会让**整段脚本**
// 不执行：按钮、事件绑定、初始数据加载全部失效，用户看到的就是
// 「能输密码但点什么都没反应」。
//
// 最容易踩的坑是把真实换行打进字符串字面量里（JS 的 "..." 不能跨行）。
// 下面这个测试在 go test 阶段就把它挡住。

const (
	jsBackslash = 0x5c // \
	jsQuote     = 0x22 // "
	jsSQuote    = 0x27 // '
	jsBacktick  = 0x60 // `
)

// TestConsoleScriptHasNoMultilineString 扫描内嵌控制台 HTML 的 <script> 区间，
// 找出「双引号数量为奇数」的行 —— 即字符串跨行。
func TestConsoleScriptHasNoMultilineString(t *testing.T) {
	lines := strings.Split(string(consoleHTML), "\n")

	start, end := -1, -1
	for i, line := range lines {
		if start < 0 && strings.Contains(line, "<script") {
			start = i
		}
		if strings.Contains(line, "</script>") {
			end = i
		}
	}
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("没找到 <script> 区块（start=%d end=%d）", start, end)
	}

	var bad []int
	for i := start + 1; i < end; i++ {
		if countDoubleQuotes(stripLiteralsAndComments(lines[i]))%2 == 1 {
			bad = append(bad, i+1)
		}
	}
	if len(bad) > 0 {
		t.Errorf("第 %v 行有跨行的双引号字符串 —— 整段 <script> 会 SyntaxError，"+
			"页面上所有按钮与事件绑定都会失效", bad)
		for _, ln := range bad {
			t.Logf("line %d: %s", ln, lines[ln-1])
		}
	}
}

// TestConsoleScriptBalanced 粗略校验括号 / 花括号 / 方括号配对，
// 以及没有被提前闭合的 <script>。
func TestConsoleScriptBalanced(t *testing.T) {
	html := string(consoleHTML)
	if strings.Count(html, "<script") != 1 || strings.Count(html, "</script>") != 1 {
		t.Fatalf("<script> 数量异常: %d 开 / %d 闭",
			strings.Count(html, "<script"), strings.Count(html, "</script>"))
	}

	body := html[strings.Index(html, "<script"):]
	body = body[strings.Index(body, ">")+1:]
	body = body[:strings.Index(body, "</script>")]

	depth := map[byte]int{}
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
			depth[c]++
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
