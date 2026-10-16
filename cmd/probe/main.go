// Command probe 是本机诊断工具：检查 CodeArts Agent 凭据是否可读、
// 上游是否可达、模型清单与额度是否正常。
//
// 用法:
//
//	go run ./cmd/probe                  # 全量诊断
//	go run ./cmd/probe -user-data <dir> # 指定 User Data 目录
//	go run ./cmd/probe -dump-keys       # 只列出 state.vscdb 中 ItemTable 的键名
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"codearts2api/internal/codearts"
)

func main() {
	var (
		userData = flag.String("user-data", "", "CodeArts Agent 的 User Data 目录")
		dumpKeys = flag.Bool("dump-keys", false, "只列出 state.vscdb 中 ItemTable 的键名")
		model    = flag.String("model", "glm-5.3-flash", "探活使用的模型")
	)
	flag.Parse()

	if *dumpKeys {
		codearts.DumpItemKeys(os.Stdout, *userData)
		return
	}

	dirs := codearts.DefaultUserDataDirs(*userData)
	fmt.Println("候选 User Data 目录:")
	for _, d := range dirs {
		ok := "不存在"
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			ok = "存在"
		}
		fmt.Printf("  - %s (%s)\n", d, ok)
	}

	var sess *codearts.Session
	for _, d := range dirs {
		s, err := codearts.ReadSession(d)
		if err != nil {
			fmt.Printf("\n[%s] 读取失败: %v\n", d, err)
			continue
		}
		fmt.Printf("\n[%s] 读取成功\n", d)
		fmt.Printf("  账号      : %s\n", s.AccountLabel)
		fmt.Printf("  AccessKey : %s\n", codearts.Mask(s.AccessKey))
		fmt.Printf("  SecurityToken: %v\n", s.SecurityToken != "")
		fmt.Printf("  有效期至  : %s（剩余 %s）\n",
			s.ExpiresAt.Local().Format("2006-01-02 15:04:05"),
			s.ExpiresIn().Round(time.Second))
		fmt.Printf("  可续期    : %v\n", s.HasRefresh())
		if s.HasRefresh() {
			if jkt, err := codearts.JWKThumbprint(s.DPoPPublicJWK); err == nil {
				fmt.Printf("  DPoP jkt  : %s\n", jkt)
			}
		}
		sess = s
		break
	}
	if sess == nil {
		log.Fatal("没有读到任何可用会话")
	}

	client := codearts.NewClient()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Println("\n== 上游模型清单 ==")
	if cfg, err := client.FetchGatewayConfig(ctx, sess); err != nil {
		fmt.Println("  失败:", err)
	} else {
		fmt.Println("  base_url:", cfg.BaseURL)
		for _, m := range cfg.Models {
			fmt.Printf("  - %-26s ctx=%-9d max=%-8d sort=%d\n",
				m.ModelID, m.ContextWindow, m.MaxTokens, m.Sort)
		}
	}

	fmt.Println("\n== 内置模型清单 ==")
	if list, err := client.FetchBuiltinModels(ctx, sess); err != nil {
		fmt.Println("  失败:", err)
	} else {
		for _, m := range list {
			fmt.Printf("  - %-24s %-10s enable=%v display=%v img=%v\n",
				m.ModelID, m.ModelCategory, m.Enable, m.DisplayEnabled, m.SupportsImages)
		}
	}

	fmt.Println("\n== 额度 ==")
	if bal, err := client.FetchBalance(ctx, sess); err != nil {
		fmt.Println("  失败:", err)
	} else {
		fmt.Printf("  通道 %s：今日 %d / %d，剩余 %d\n",
			bal.Channel, bal.DailyTokensUsed, bal.DailyTokenLimit, bal.TotalBalance)
	}

	fmt.Printf("\n== 对话探活（%s）==\n", *model)
	if err := client.Probe(ctx, sess, *model); err != nil {
		fmt.Println("  失败:", err)
	} else {
		fmt.Println("  OK")
	}
}
