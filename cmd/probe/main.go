// Command probe 是本机诊断工具：检查 CodeArts Agent 凭据是否可读、
// 上游是否可达、可用模型清单与运营活动是否正常。
//
// 用法:
//
//	go run ./cmd/probe                        # 全量诊断（凭据 / 模型清单 / 运营活动，不消耗积分）
//	go run ./cmd/probe -user-data <dir>       # 指定 User Data 目录
//	go run ./cmd/probe -dump-keys             # 只列出 state.vscdb 中 ItemTable 的键名
//	go run ./cmd/probe -model glm-5.3         # 额外做对话探活（会消耗少量积分）
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
		model    = flag.String("model", "", "探活使用的模型（留空则跳过对话探活；探活会消耗少量积分）")
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

	showBuiltin(ctx, client, sess)
	showWelfare(ctx, client, sess)

	if *model == "" {
		return
	}
	fmt.Printf("\n== 对话探活（%s）==\n", *model)
	fmt.Println("  注意：免费权益通道已下线，探活会消耗极少量积分（max_tokens=8）")
	if err := client.Probe(ctx, sess, *model); err != nil {
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

// showBuiltin 列出可用模型清单（积分通道）。
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

// showWelfare 列出运营活动（「活动福利」）及可领取状态。
func showWelfare(ctx context.Context, c *codearts.Client, sess *codearts.Session) {
	fmt.Println("\n== 运营活动（活动福利）==")
	d, err := c.FetchWelfareDelivery(ctx, sess)
	if err != nil {
		fmt.Println("  失败:", err)
		return
	}
	for _, w := range d.Campaigns {
		auto := "否"
		if w.IsAutoClaimable() {
			auto = "是"
		}
		fmt.Printf("  - id=%-2d %-22s type=%-18s status=%-10s 可领=%-5v 可自动=%s\n",
			w.CampaignID, w.Title, w.Type, w.Status, w.Claimable, auto)
		fmt.Printf("        奖励 %.0f %s  触发 %s  有效期 %s ~ %s\n",
			w.BenefitAmount, w.BenefitUnit, w.Extra.TriggerEvent,
			cut10(w.Extra.StartTime), cut10(w.Extra.EndTime))
	}
	if len(d.Claimable()) == 0 {
		fmt.Println("  （当前没有可自动领取的活动）")
	}
}

// cut10 截取时间串前 10 位（日期部分）。
func cut10(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}
