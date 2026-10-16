package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"codearts2api/internal/codearts"
	"codearts2api/internal/registry"
	"codearts2api/internal/store"
)

// handleAdminState 返回控制台首屏所需的全部状态。
func (s *Server) handleAdminState(w http.ResponseWriter, r *http.Request) {
	accts := s.Store.Accounts()
	safe := make([]map[string]any, 0, len(accts))
	for _, a := range accts {
		row := map[string]any{
			"id": a.ID, "name": a.DisplayName(), "source": a.Source,
			"user_data": a.UserData, "enabled": a.Enabled,
			"status": a.Status, "note": a.Note, "last_err": a.LastErr,
			"failures": a.Failures, "last_used": a.LastUsed,
		}
		if a.Session != nil {
			row["access_key"] = codearts.Mask(a.Session.AccessKey)
			row["expires_at"] = a.Session.ExpiresAt.Unix()
			row["expires_in"] = int(a.Session.ExpiresIn().Seconds())
			row["has_refresh"] = a.Session.HasRefresh()
			row["domain_id"] = codearts.Mask(a.Session.DomainID)
		}
		safe = append(safe, row)
	}

	keys := s.Store.Keys()
	safeKeys := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		safeKeys = append(safeKeys, map[string]any{
			"key": codearts.Mask(k.Key), "name": k.Name, "enabled": k.Enabled,
			"created_at": k.CreatedAt, "last_used": k.LastUsed, "requests": k.Requests,
		})
	}

	s.cache.mu.RLock()
	balance, balanceAt, cacheErr := s.cache.balance, s.cache.balanceAt, s.cache.err
	s.cache.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       safe,
		"keys":           safeKeys,
		"models":         s.Reg.List(s.Store.Settings().ModelAliases),
		"settings":       sanitizedSettings(s.Store.Settings()),
		"stats":          s.Store.Stats(),
		"balance":        balance,
		"balance_at":     balanceAt.Unix(),
		"upstream_error": cacheErr,
	})
}

// handleAdminAccounts 处理账号池操作。
func (s *Server) handleAdminAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	var body struct {
		Action        string `json:"action"`
		ID            string `json:"id"`
		Dir           string `json:"dir"`
		Name          string `json:"name"`
		AccessKey     string `json:"access_key"`
		SecretKey     string `json:"secret_key"`
		SecurityToken string `json:"security_token"`
		SessionJSON   string `json:"session_json"`
		ExpiresAt     int64  `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败", "invalid_request_error")
		return
	}

	switch body.Action {
	case "import":
		dir := body.Dir
		if dir == "" {
			dir = s.Store.Settings().UserDataDir
		}
		results, err := codearts.ImportFromDesktop(dir)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "导入失败: "+err.Error(), "invalid_request_error")
			return
		}
		imported := make([]map[string]any, 0, len(results))
		for _, res := range results {
			isNew := s.Store.UpsertAccount(res.Account)
			s.Cred.Invalidate(res.Account.ID)
			imported = append(imported, map[string]any{
				"id": res.Account.ID, "name": res.Account.Name, "dir": res.Dir,
				"access_key": codearts.Mask(res.AK),
				"expires_at": res.Expires.Unix(), "new": isNew,
			})
		}
		_ = s.Store.Save()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(imported), "imported": imported})

	case "manual":
		acct, err := codearts.ManualAccount(body.Name, body.AccessKey, body.SecretKey, body.SecurityToken, unixTime(body.ExpiresAt))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		isNew := s.Store.UpsertAccount(acct)
		s.Cred.Invalidate(acct.ID)
		_ = s.Store.Save()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "new": isNew, "id": acct.ID})

	case "session-json":
		sess, err := codearts.SessionFromJSON([]byte(body.SessionJSON))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "session JSON 解析失败: "+err.Error(), "invalid_request_error")
			return
		}
		if sess.AccessKey == "" || sess.SecretKey == "" {
			writeErr(w, http.StatusBadRequest, "session JSON 里缺少 accessKey / secretKey", "invalid_request_error")
			return
		}
		name := body.Name
		if name == "" {
			name = sess.AccountLabel
		}
		acct := &codearts.Account{
			ID: codearts.RandomHex(8), Name: name, Source: "manual",
			Enabled: true, Status: "ok", Session: sess,
		}
		isNew := s.Store.UpsertAccount(acct)
		s.Cred.Invalidate(acct.ID)
		_ = s.Store.Save()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "new": isNew, "id": acct.ID})

	case "remove":
		ok := s.Store.RemoveAccount(body.ID)
		_ = s.Store.Save()
		writeJSON(w, http.StatusOK, map[string]any{"ok": ok})

	case "toggle":
		if a := s.Store.Account(body.ID); a != nil {
			a.Enabled = !a.Enabled
			_ = s.Store.Save()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "clear-errors":
		if a := s.Store.Account(body.ID); a != nil {
			a.Failures = 0
			a.Status = "ok"
			a.LastErr = ""
			_ = s.Store.Save()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "reimport":
		a := s.Store.Account(body.ID)
		if a == nil {
			writeErr(w, http.StatusBadRequest, "账号不存在", "invalid_request_error")
			return
		}
		s.Cred.Invalidate(a.ID)
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if _, err := s.Cred.Session(ctx, a, codearts.ResolveOptions{
			LiveTTL: 0, Margin: 5 * time.Minute, AllowRefresh: true, WriteBack: false,
		}); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error(), "upstream_error")
			return
		}
		_ = s.Store.Save()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		writeErr(w, http.StatusBadRequest, "未知操作: "+body.Action, "invalid_request_error")
	}
}

// handleAdminKeys 处理密钥的增删改查。
func (s *Server) handleAdminKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		keys := s.Store.Keys()
		safe := make([]map[string]any, 0, len(keys))
		for _, k := range keys {
			safe = append(safe, map[string]any{
				"key": codearts.Mask(k.Key), "name": k.Name, "enabled": k.Enabled,
				"created_at": k.CreatedAt, "last_used": k.LastUsed, "requests": k.Requests,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": safe})
	case http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		k := s.Store.AddKey(body.Name)
		_ = s.Store.Save()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": k.Key})
	case http.MethodDelete:
		q := r.URL.Query()
		if ref := q.Get("key"); ref != "" {
			_ = s.Store.DeleteKey(s.resolveKeyRef(ref))
		}
		if ref := q.Get("toggle"); ref != "" {
			_ = s.Store.ToggleKey(s.resolveKeyRef(ref))
		}
		_ = s.Store.Save()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法", "invalid_request_error")
	}
}

// handleAdminLogs 返回或清空请求日志。
func (s *Server) handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.Store.ClearLogs()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": s.Store.Logs(intQuery(r, "limit"))})
}

// handleAdminStats 返回或重置统计。
func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.Store.ClearStats()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeJSON(w, http.StatusOK, s.Store.Stats())
}

// handleAdminSettings 读取或更新设置。
func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusOK, sanitizedSettings(s.Store.Settings()))
		return
	}
	var body struct {
		ModelAliases      map[string]string `json:"model_aliases"`
		DefaultModel      *string           `json:"default_model"`
		ForceModel        *string           `json:"force_model"`
		RequestTimeout    *int              `json:"request_timeout_sec"`
		MaxRetries        *int              `json:"max_retries"`
		LogKeep           *int              `json:"log_keep"`
		CORSOrigin        *string           `json:"cors_origin"`
		AdminPassword     *string           `json:"admin_password"`
		AutoRefresh       *bool             `json:"auto_refresh"`
		AllowCreditModels *bool             `json:"allow_credit_models"`
		WriteBackToIDE    *bool             `json:"write_back_to_ide"`
		LiveTTLSeconds    *int              `json:"live_ttl_sec"`
		UserDataDir       *string           `json:"user_data_dir"`
		PluginName        *string           `json:"plugin_name"`
		PluginVersion     *string           `json:"plugin_version"`
		IDEName           *string           `json:"ide_name"`
		IDEVersion        *string           `json:"ide_version"`
		Language          *string           `json:"language"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败", "invalid_request_error")
		return
	}
	s.Store.UpdateSettings(func(st *store.Settings) {
		if body.ModelAliases != nil {
			st.ModelAliases = body.ModelAliases
		}
		setIf(&st.DefaultModel, body.DefaultModel)
		setIf(&st.ForceModel, body.ForceModel)
		setIf(&st.RequestTimeout, body.RequestTimeout)
		setIf(&st.MaxRetries, body.MaxRetries)
		setIf(&st.LogKeep, body.LogKeep)
		setIf(&st.CORSOrigin, body.CORSOrigin)
		setIf(&st.AutoRefresh, body.AutoRefresh)
		setIf(&st.AllowCreditModels, body.AllowCreditModels)
		setIf(&st.WriteBackToIDE, body.WriteBackToIDE)
		setIf(&st.LiveTTLSeconds, body.LiveTTLSeconds)
		setIf(&st.UserDataDir, body.UserDataDir)
		setIf(&st.PluginName, body.PluginName)
		setIf(&st.PluginVersion, body.PluginVersion)
		setIf(&st.IDEName, body.IDEName)
		setIf(&st.IDEVersion, body.IDEVersion)
		setIf(&st.Language, body.Language)
		if body.AdminPassword != nil {
			st.AdminPassword = strings.TrimSpace(*body.AdminPassword)
		}
	})
	_ = s.Store.Save()
	// 身份头变更后重建上游客户端
	s.ApplyIdentity()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": sanitizedSettings(s.Store.Settings())})
}

// ApplyIdentity 把设置里的身份头同步到上游客户端。
func (s *Server) ApplyIdentity() {
	st := s.Store.Settings()
	id := codearts.DefaultIdentity()
	if st.PluginName != "" {
		id.PluginName = st.PluginName
	}
	if st.PluginVersion != "" {
		id.PluginVersion = st.PluginVersion
	}
	if st.IDEName != "" {
		id.IDEName = st.IDEName
	}
	if st.IDEVersion != "" {
		id.IDEVersion = st.IDEVersion
	}
	if st.Language != "" {
		id.Language = st.Language
	}
	s.Up.Identity = id
}

// handleAdminCheckin 对全部（或指定）账号做一次连通性探测。
func (s *Server) handleAdminCheckin(w http.ResponseWriter, r *http.Request) {
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
	results := make([]map[string]any, 0, len(targets))
	for _, a := range targets {
		s.Cred.Invalidate(a.ID)
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		sess, err := s.Cred.Session(ctx, a, s.resolveOptions())
		if err == nil {
			probe := s.defaultProbeModel()
			err = s.Up.Probe(ctx, sess, probe.ID, probe.Channel)
		}
		cancel()
		ok := err == nil
		note := ""
		if err != nil {
			note = err.Error()
		}
		s.Store.MarkAccountResult(a.ID, ok, note)
		row := map[string]any{"id": a.ID, "name": a.DisplayName(), "ok": ok, "err": note}
		if sess != nil {
			row["expires_at"] = sess.ExpiresAt.Unix()
			row["access_key"] = codearts.Mask(sess.AccessKey)
		}
		results = append(results, row)
	}
	_ = s.Store.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
}

// defaultProbeModel 返回探活用的模型。
//
// 探活永远走免费权益通道的模型，避免「点一下探活就扣积分」。
func (s *Server) defaultProbeModel() registry.Model {
	st := s.Store.Settings()
	m := s.Reg.Resolve(st.DefaultModel, st.ModelAliases)
	if m.IsCredit() {
		m = s.Reg.Resolve("glm-5.3-flash", st.ModelAliases)
	}
	return m
}

// setIf 仅在指针非空时赋值。
func setIf[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

// unixTime 把 Unix 秒转换为时间（0 表示使用默认有效期）。
func unixTime(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}
