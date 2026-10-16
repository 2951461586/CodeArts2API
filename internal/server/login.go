package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"codearts2api/internal/codearts"
)

// --- 每日签到 ---

// handleAdminClaim 手动触发一次签到（全部账号或指定账号）。
func (s *Server) handleAdminClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	targets := s.Store.Accounts()
	if body.ID != "" {
		targets = nil
		if a := s.Store.Account(body.ID); a != nil {
			targets = append(targets, a)
		}
	}
	today := time.Now().Format("2006-01-02")
	results := make([]map[string]any, 0, len(targets))
	okCount := 0
	for _, a := range targets {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		msg, err := s.ClaimForAccount(ctx, a, today)
		cancel()
		row := map[string]any{"id": a.ID, "name": a.DisplayName(), "ok": err == nil}
		if err != nil {
			row["message"] = codearts.DescribeClaimError(err)
		} else {
			row["message"] = msg
			okCount++
		}
		results = append(results, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "date": today, "succeeded": okCount, "total": len(results), "results": results,
	})
}

// --- 本地浏览器登录 ---

// handleAdminLoginStart 启动一次浏览器登录，返回待打开的授权 URL。
func (s *Server) handleAdminLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	var body struct {
		PortalHost string `json:"portal_host"`
		Locale     string `json:"locale"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	st := s.Store.Settings()
	flow, err := s.Logins.Start(codearts.LoginOptions{
		PortalHost: body.PortalHost,
		Locale:     body.Locale,
		Port:       st.LoginPort,
		// 上游身份头里的 ide-version 就是 CodeArts Agent 的版本，登录时上报同一个值
		PluginVersion: st.IDEVersion,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "启动登录失败: "+err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"session_id":  flow.ID,
		"auth_url":    flow.AuthURL,
		"port":        flow.Port,
		"callback":    "http://127.0.0.1:" + strconv.Itoa(flow.Port) + codearts.AuthRedirectPath,
		"expires_in":  int(5 * time.Minute / time.Second),
		"hint":        flow.CallbackHint(),
		"tunnel_hint": "ssh -N -L " + strconv.Itoa(flow.Port) + ":127.0.0.1:" + strconv.Itoa(flow.Port) + " <user>@<server>",
	})
}

// handleAdminLoginPoll 轮询登录状态；成功后自动把账号写入账号池。
func (s *Server) handleAdminLoginPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
		Name      string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SessionID == "" {
		writeErr(w, http.StatusBadRequest, "缺少 session_id", "invalid_request_error")
		return
	}
	flow := s.Logins.Get(body.SessionID)
	if flow == nil {
		writeErr(w, http.StatusNotFound, "登录会话不存在或已过期", "invalid_request_error")
		return
	}

	status, msg, account, sess := flow.Snapshot()
	resp := map[string]any{"status": string(status), "message": msg}

	if status == codearts.LoginConfirmed && sess != nil {
		if flow.MarkSaved() {
			name := body.Name
			if name == "" {
				name = account
			}
			acct := &codearts.Account{
				ID:      codearts.RandomHex(8),
				Name:    name,
				Source:  "browser",
				Enabled: true,
				Status:  "ok",
				Session: sess,
			}
			isNew := s.Store.UpsertAccount(acct)
			s.persist()
			resp["account_id"] = acct.ID
			resp["account_name"] = acct.DisplayName()
			resp["new"] = isNew
		}
		resp["account"] = account
		resp["access_key"] = codearts.Mask(sess.AccessKey)
		resp["expires_at"] = sess.ExpiresAt.Unix()
		resp["has_refresh"] = sess.HasRefresh()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAdminLoginCallback 接收用户手工粘贴的回调 URL（远端部署无隧道时的兜底）。
func (s *Server) handleAdminLoginCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
		URL       string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败", "invalid_request_error")
		return
	}
	if body.SessionID == "" || strings.TrimSpace(body.URL) == "" {
		writeErr(w, http.StatusBadRequest, "缺少 session_id 或 url", "invalid_request_error")
		return
	}
	flow := s.Logins.Get(body.SessionID)
	if flow == nil {
		writeErr(w, http.StatusNotFound, "登录会话不存在或已过期，请重新发起登录", "invalid_request_error")
		return
	}
	code, err := flow.SubmitCallbackURL(body.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"accepted": true,
		"code_len": len(code),
		"message":  "已收到授权码，正在换取凭据…",
	})
}

// handleAdminLoginCancel 取消一次登录。
func (s *Server) handleAdminLoginCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	ok := s.Logins.Cancel(body.SessionID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok})
}
