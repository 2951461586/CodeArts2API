package codearts

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DumpItemKeys 列出 state.vscdb 中 ItemTable 的全部键名，用于排查
// 「secret 键名变了」这类问题（不需要解密，因此跨平台可用）。
func DumpItemKeys(w io.Writer, explicit string) {
	for _, dir := range DefaultUserDataDirs(explicit) {
		dbPath := filepath.Join(dir, "User", "globalStorage", "state.vscdb")
		fmt.Fprintf(w, "\n[%s]\n", dbPath)
		data, err := os.ReadFile(dbPath)
		if err != nil {
			fmt.Fprintf(w, "  读取失败: %v\n", err)
			continue
		}
		db, err := openSQLite(data)
		if err != nil {
			fmt.Fprintf(w, "  解析失败: %v\n", err)
			continue
		}
		root, err := db.tableRoot("ItemTable")
		if err != nil {
			fmt.Fprintf(w, "  未找到 ItemTable: %v\n", err)
			continue
		}
		rows, err := db.scanTree(root)
		if err != nil {
			fmt.Fprintf(w, "  扫描失败: %v\n", err)
			continue
		}
		keys := make([]string, 0, len(rows))
		for _, r := range rows {
			if len(r) < 2 {
				continue
			}
			keys = append(keys, r[0].AsString())
		}
		sort.Strings(keys)
		fmt.Fprintf(w, "  共 %d 行：\n", len(keys))
		for _, k := range keys {
			mark := "  "
			if strings.Contains(k, "HuaweiCloud") || strings.Contains(k, "huaweicloud") {
				mark = "→ "
			}
			if len(k) > 140 {
				k = k[:140] + "…"
			}
			fmt.Fprintf(w, "  %s%s\n", mark, k)
		}
	}
}
