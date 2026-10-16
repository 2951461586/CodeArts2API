// Command probe 是本机诊断工具：检查 CodeArts Agent 凭据是否可读、
// 上游两条通道（免费权益 / 积分）是否可达、模型清单与额度是否正常。
//
// 用法:
//
//	go run ./cmd/probe                        # 全量诊断（默认探活免费通道）
//	go run ./cmd/probe -user-data <dir>       # 指定 User Data 目录
//	go run ./cmd/probe -dump-keys             # 只列出 state.vscdb 中 ItemTable 的键名
//	go run ./cmd/probe -model GLM-5.2         # 探活指定模型（自动判断通道）
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"codearts2api/internal/codearts"
)

func main() {
	var (
		userData = flag.String("user-data", "", "CodeArts Agent 的 User Data 目录")
		dumpKeys = flag.Bool("dump-keys", false, "只列出 state.vscdb 中 ItemTable 的键名")
		model    = flag.String("model", "glm-5.3-flash", "探活使用的模型（留空则跳过对话探活）")
	)
	flag.Parse()

	if *dumpKeys {
		codearts.DumpItemKeys(os.Stdout, *userData)
		return
	}

	dirs := codearts.DefaultUserDataDirs(*userData)
	fmt.Println("候选 User Data 目录:")
	for _, d := range dirs {
		state := "不存在"
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			state = "存在"
		}
		fmt.Printf("  - %s (%s)\n", d, state)
	}

	sess := readAny(dirs)
	if sess == nil {
		log.Fatal("没有读到任何可用会话")
	}

	client := codearts.NewClient()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	benefit := showBenefit(ctx, client, sess)
	showBuiltin(ctx, client, sess)
	showBalance(ctx, client, sess)

	if *model == "" {
		return
	}
	ch := channelFor(benefit, *model)
	fmt.Printf("\n== 对话探活（%s，通道 %s）==\n", *model, ch)
	if ch == codearts.ChannelCredit {
		fmt.Println("  注意：积分通道会消耗账号积分")
	}
	if err := client.Probe(ctx, sess, *model, ch); err != nil {
		fmt.Println("  失败:", err)
	} else {
		fmt.Println("  OK")
	}
}

// readAny 依次尝试各个 User Data 目录，返回第一个可读的会话。
func readAny(dirs []string) *codearts.Session {
	for _, d := range dirs {
		s, err := codearts.ReadSession(d)
		if err != nil {
			fmt.Printf("\n[%s] 读取失败: %v\n", d, err)
			continue
		}
		fmt.Printf("\n[%s] 读取成功\n", d)
		fmt.Printf("  账号          : %s\n", s.AccountLabel)
		fmt.Printf("  AccessKey     : %s\n", codearts.Mask(s.AccessKey))
		fmt.Printf("  SecurityToken : %v\n", s.SecurityToken != "")
		fmt.Printf("  有效期至      : %s（剩余 %s）\n",
			s.ExpiresAt.Local().Format("2006-01-02 15:04:05"),
			s.ExpiresIn().Round(time.Second))
		fmt.Printf("  可续期        : %v\n", s.HasRefresh())
		if s.HasRefresh() {
			if jkt, err := codearts.JWKThumbprint(s.DPoPPublicJWK); err == nil {
				fmt.Printf("  DPoP jkt      : %s\n", jkt)
			}
		}
		return s
	}
	return nil
}

// showBenefit 列出免费权益通道的模型。
func showBenefit(ctx context.Context, c *codearts.Client, sess *codearts.Session) []codearts.GatewayModel {
	fmt.Println("\n== benefit 通道（免费额度，请求头带 maas_type: benefit）==")
	cfg, err := c.FetchGatewayConfig(ctx, sess)
	if err != nil {
		fmt.Println("  失败:", err)
		return nil
	}
	fmt.Println("  base_url:", cfg.BaseURL)
	for _, m := range cfg.Models {
		fmt.Printf("  - %-26s ctx=%-9d max=%-8d sort=%d\n",
			m.ModelID, m.ContextWindow, m.MaxTokens, m.Sort)
	}
	return cfg.Models
}

// showBuiltin 列出积分通道的内置模型。
func showBuiltin(ctx context.Context, c *codearts.Client, sess *codearts.Session) {
	fmt.Println("\n== 内置模型清单（积分通道，请求头不带 maas_type）==")
	list, err := c.FetchBuiltinModels(ctx, sess)
	if err != nil {
		fmt.Println("  失败:", err)
		return
	}
	for _, m := range list {
		credit := m.CreditRatio()
		if credit == "" {
			credit = "—"
		}
		fmt.Printf("  - %-24s %-10s enable=%-5v display=%-5v img=%-5v 积分倍率=%s\n",
			m.ModelID, m.ModelCategory, m.Enable, m.DisplayEnabled, m.SupportsImages, credit)
	}
}

// showBalance 展示免费权益额度。
func showBalance(ctx context.Context, c *codearts.Client, sess *codearts.Session) {
	fmt.Println("\n== 免费权益额度 ==")
	bal, err := c.FetchBalance(ctx, sess)
	if err != nil {
		fmt.Println("  失败:", err)
		return
	}
	fmt.Printf("  通道 %s：今日 %d / %d，剩余 %d\n",
		bal.Channel, bal.DailyTokensUsed, bal.DailyTokenLimit, bal.TotalBalance)
}

// channelFor 判断某模型名该走哪条通道：
// 出现在 benefit 清单里的走免费额度，其余（如 GLM-5.3 / GLM-5.2）走积分通道。
func channelFor(benefit []codearts.GatewayModel, model string) codearts.Channel {
	for _, m := range benefit {
		if strings.EqualFold(m.ModelID, model) {
			return codearts.ChannelBenefit
		}
	}
	return codearts.ChannelCredit
}
