package server

import (
	"context"
	"log"
	"time"

	"codearts2api/internal/codearts"
)

// 每日免费福利 Token 的自动签到。
//
// 官方活动说明「登录成功后客户端会自动为你领取该福利」，所以 IDE 运行时
// 其实已经替我们领了。网关的自动签到是这三种情况的兜底：
//   - IDE 没开（网关独立跑）
//   - 跨天（福利 Token 当日 24:00 清零，第二天需要重新领）
//   - 多账号（IDE 只登录了一个账号）
//
// 领取接口是幂等的：当天已领过再领一次仍返回 success，额度不变。

// claimTickInterval 是签到检查的间隔。
//
// 用「定时轮询 + 按本地日期去重」而不是「算准 00:05 触发一次」，
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

// ClaimForAccount 为单个账号领取当日福利 Token。
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

	res, err := s.Up.ClaimDaily(ctx, sess)
	if err != nil {
		msg := codearts.DescribeClaimError(err)
		s.Store.MarkClaimResult(a.ID, today, false, msg)
		_ = s.Store.Save()
		return "", err
	}

	s.Store.MarkClaimResult(a.ID, today, res.OK, res.Message)
	_ = s.Store.Save()
	return res.Message, nil
}
