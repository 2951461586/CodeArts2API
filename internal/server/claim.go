package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"codearts2api/internal/codearts"
)

// 每日自动签到。
//
// 「签到」实际包含两块，都按本地日期各领一次：
//
//  1. 免费福利 Token —— GET /api/v1/benefit/claim（每日 1000 万 tokens，24:00 清零）
//  2. 运营活动奖励   —— GET /v1/ops/delivery + POST /v1/ops/claim
//     线上目前有「每日签到领1000积分」（campaignId=1）等
//
// 官方活动说明「登录成功后客户端会自动为你领取该福利」，所以 IDE 运行时
// 其实已经替我们领了。网关的自动签到是这三种情况的兜底：
//   - IDE 没开（网关独立跑）
//   - 跨天（福利当日 24:00 清零，第二天需要重新领）
//   - 多账号（IDE 只登录了一个账号）
//
// 两个接口都是幂等的，重复调用没有副作用。
//
// **只领服务端说能领的**：邀请奖励（推荐码上有礼）与学生认证奖励必须由
// 真实事件触发（好友真实注册 / 真实学生认证），服务端会直接拒绝直接领取，
// 网关也绝不会伪造触发事件。

// claimTickInterval 是签到检查的间隔。
//
// 用「定时轮询 + 按本地日期去重」而不是「算准时刻触发一次」，
// 这样可以避开休眠唤醒、时区切换、进程重启导致的漏签。
const claimTickInterval = 5 * time.Minute

// StartClaimLoop 启动后台自动签到。
func (s *Server) StartClaimLoop(ctx context.Context) {
	go func() {
		// 启动后先延迟几秒，等账号导入与模型刷新完成
		select {
		case <-ctx.Done():
			return
		case <-time.After(8 * time.Second):
		}
		s.RunDailyClaim(ctx)

		ticker := time.NewTicker(claimTickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.RunDailyClaim(ctx)
			}
		}
	}()
}

// RunDailyClaim 对尚未签到过的账号各领一次当日福利。
func (s *Server) RunDailyClaim(ctx context.Context) {
	st := s.Store.Settings()
	if !st.AutoClaim {
		return
	}
	now := time.Now()
	if !claimWindowOpen(now, st.ClaimAfterHour, st.ClaimAfterMinute) {
		return
	}
	today := now.Format("2006-01-02")
	for _, a := range s.Store.Accounts() {
		if !a.Enabled || a.LastClaimDate == today {
			continue
		}
		if _, err := s.ClaimForAccount(ctx, a, today); err != nil {
			log.Printf("账号 %s 自动签到失败: %s", a.DisplayName(), codearts.DescribeClaimError(err))
		}
	}
}

// claimWindowOpen 报告当前是否已过每日签到时刻。
func claimWindowOpen(now time.Time, hour, minute int) bool {
	cur := now.Hour()*60 + now.Minute()
	return cur >= hour*60+minute
}

// ClaimForAccount 为单个账号执行一次完整签到（免费 Token + 运营活动）。
//
// 返回给控制台展示的一句话。today 为空时按当前本地日期计算。
func (s *Server) ClaimForAccount(ctx context.Context, a *codearts.Account, today string) (string, error) {
	if today == "" {
		today = time.Now().Format("2006-01-02")
	}
	s.Cred.Invalidate(a.ID)

	sess, err := s.Cred.Session(ctx, a, s.resolveOptions())
	if err != nil {
		s.Store.MarkClaimResult(a.ID, today, false, err.Error())
		_ = s.Store.Save()
		return "", err
	}

	var (
		parts    []string
		firstErr error
	)

	// 1) 免费福利 Token
	if res, err := s.Up.ClaimDaily(ctx, sess); err != nil {
		firstErr = err
		parts = append(parts, "免费 Token："+codearts.DescribeClaimError(err))
	} else if res.OK {
		parts = append(parts, "免费 Token：已领（每日 1000 万）")
	} else {
		parts = append(parts, "免费 Token："+res.Message)
	}

	// 2) 运营活动（每日积分等）
	if results, err := s.Up.ClaimDailyWelfare(ctx, sess); err != nil {
		if firstErr == nil {
			firstErr = err
		}
		parts = append(parts, "活动福利："+err.Error())
	} else if len(results) == 0 {
		parts = append(parts, "活动福利：暂无可领")
	} else {
		for _, r := range results {
			parts = append(parts, codearts.DescribeWelfareError(r))
		}
	}

	msg := strings.Join(parts, "；")
	// 只要有一项成功就记下当天日期，避免同一轮反复重试
	ok := firstErr == nil
	s.Store.MarkClaimResult(a.ID, today, ok, msg)
	_ = s.Store.Save()

	if !ok {
		return msg, firstErr
	}
	return msg, nil
}

// WelfareSnapshot 返回某账号的活动福利清单（供控制台展示）。
func (s *Server) WelfareSnapshot(ctx context.Context, a *codearts.Account) (*codearts.WelfareDelivery, error) {
	sess, err := s.Cred.Session(ctx, a, s.resolveOptions())
	if err != nil {
		return nil, err
	}
	return s.Up.FetchWelfareDelivery(ctx, sess)
}

// handleAdminWelfare 返回运营活动清单（「活动福利」）。
func (s *Server) handleAdminWelfare(w http.ResponseWriter, r *http.Request) {
	acct := s.Store.NextAccount()
	if acct == nil {
		writeErr(w, http.StatusBadRequest, errNoAccount.Error(), "invalid_request_error")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	delivery, err := s.WelfareSnapshot(ctx, acct)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "获取活动列表失败: "+err.Error(), "upstream_error")
		return
	}

	items := make([]map[string]any, 0, len(delivery.Campaigns))
	for _, c := range delivery.Campaigns {
		items = append(items, map[string]any{
			"campaign_id":    c.CampaignID,
			"title":          c.Title,
			"type":           c.Type,
			"status":         c.Status,
			"claimable":      c.Claimable,
			"auto_claimable": c.IsAutoClaimable(),
			"benefit_amount": c.BenefitAmount,
			"benefit_unit":   c.BenefitUnit,
			"description":    c.Description,
			"trigger_mode":   c.Extra.TriggerMode,
			"trigger_event":  c.Extra.TriggerEvent,
			"start_time":     c.Extra.StartTime,
			"end_time":       c.Extra.EndTime,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"account":   acct.DisplayName(),
		"campaigns": items,
	})
}

// handleAdminInvite 生成（或取回）专属邀请码与邀请链接。
//
// 只负责把链接取出来给用户 —— 邀请奖励必须由真实好友通过链接完成注册才会
// 产生，服务端对直接领取邀请活动会返回「需要先通过触发接口激活」。
func (s *Server) handleAdminInvite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	var body struct {
		CampaignID int    `json:"campaign_id"`
		ID         string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.CampaignID == 0 {
		body.CampaignID = 3 // 「推荐码上有礼」
	}

	var acct *codearts.Account
	if body.ID != "" {
		acct = s.Store.Account(body.ID)
	}
	if acct == nil {
		acct = s.Store.NextAccount()
	}
	if acct == nil {
		writeErr(w, http.StatusBadRequest, errNoAccount.Error(), "invalid_request_error")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	sess, err := s.Cred.Session(ctx, acct, s.resolveOptions())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}
	code, err := s.Up.GenerateInviteCode(ctx, sess, body.CampaignID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "生成邀请链接失败: "+err.Error(), "upstream_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"account":          acct.DisplayName(),
		"campaign_id":      code.CampaignID,
		"invite_code":      code.InviteCode,
		"invite_url":       code.InviteURL,
		"valid":            code.Valid,
		"status":           code.Status,
		"activation_count": code.ActivationCount,
		"max_activations":  code.MaxActivations,
		"hint":             "奖励需好友通过该链接完成注册后才会到账；直接领取邀请活动会被服务端拒绝",
	})
}
