package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"codearts2api/internal/apierr"
	"codearts2api/internal/codearts"
	"codearts2api/internal/registry"
	"codearts2api/internal/store"
)

// handleAdminState 返回控制台首屏所需的全部状态。
func (s *Server) handleAdminState(w http.ResponseWriter, r *http.Request) {
	today := time.Now().Format("2006-01-02")
	accts := s.Store.Accounts()
	safe := make([]map[string]any, 0, len(accts))
	for _, a := range accts {
		row := map[string]any{
			"id": a.ID, "name": a.DisplayName(), "source": a.Source,
			"user_data": a.UserData, "enabled": a.Enabled,
			"status": a.Status, "note": a.Note, "last_err": a.LastErr,
			"failures": a.Failures, "last_used": a.LastUsed,
			"last_claim_date": a.LastClaimDate, "last_claim_at": a.LastClaimAt,
			"last_claim_err": a.LastClaimErr,
			"last_claim_msg": a.LastClaimMsg,
			"claimed_today":  a.LastClaimDate == today,
		}
		if a.Session != nil {
			row["access_key"] = codearts.Mask(a.Session.AccessKey)
			row["expires_at"] = a.Session.ExpiresAt.Unix()
			row["expires_in"] = int(a.Session.ExpiresIn().Seconds())
			row["has_refresh"] = a.Session.HasRefresh()
			row["domain_id"] = codearts.Mask(a.Session.DomainID)
			// 永久 AK/SK 没有过期时间，控制台据此显示「永不过期」而不是 1970。
			row["permanent"] = a.Session.IsPermanent()
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
	cacheErr := s.cache.err
	s.cache.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"today":          today,
		"accounts":       safe,
		"keys":           safeKeys,
		"models":         s.Reg.List(s.Store.Settings().ModelAliases),
		"settings":       sanitizedSettings(s.Store.Settings()),
		"stats":          s.Store.Stats(),
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
		// CredentialKind 显式声明凭据形态：留空或 "sts" 走临时凭据，
		// "aksk" 表示 IAM 永久 AK/SK（不过期、不续期、不带 security_token）。
		CredentialKind string `json:"credential_kind"`
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
		s.persist()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(imported), "imported": imported})

	case "manual":
		// 永久 AK/SK：不过期、不续期，忽略 security_token 与 expires_at。
		if strings.EqualFold(strings.TrimSpace(body.CredentialKind), string(codearts.CredentialKindAKSK)) {
			acct, err := codearts.ManualAKSKAccount(body.Name, body.AccessKey, body.SecretKey)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
				return
			}
			isNew := s.Store.UpsertAccount(acct)
			s.Cred.Invalidate(acct.ID)
			s.persist()
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "new": isNew, "id": acct.ID, "credential_kind": "aksk",
			})
			return
		}
		acct, err := codearts.ManualAccount(body.Name, body.AccessKey, body.SecretKey, body.SecurityToken, unixTime(body.ExpiresAt))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		isNew := s.Store.UpsertAccount(acct)
		s.Cred.Invalidate(acct.ID)
		s.persist()
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
		s.persist()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "new": isNew, "id": acct.ID})

	case "remove":
		ok := s.Store.RemoveAccount(body.ID)
		s.persist()
		writeJSON(w, http.StatusOK, map[string]any{"ok": ok})

	case "toggle":
		a := s.Store.Account(body.ID)
		if a == nil {
			writeErr(w, http.StatusBadRequest, "账号不存在", "invalid_request_error")
			return
		}
		s.Store.SetAccountEnabled(a.ID, !a.Enabled)
		s.persist()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "clear-errors":
		if s.Store.ClearAccountErrors(body.ID) {
			s.persist()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "export":
		a := s.Store.Account(body.ID)
		if a == nil {
			writeErr(w, http.StatusBadRequest, "账号不存在", "invalid_request_error")
			return
		}
		s.Cred.Invalidate(a.ID)
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		sess, err := s.Cred.Session(ctx, a, s.resolveOptions())
		if err != nil {
			writeErr(w, http.StatusBadGateway, "导出失败: "+err.Error(), "upstream_error")
			return
		}
		plain, err := sess.RawJSON()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "序列化失败: "+err.Error(), "server_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"name":         a.DisplayName(),
			"session_json": string(plain),
			"expires_at":   sess.ExpiresAt.Unix(),
			"has_refresh":  sess.HasRefresh(),
			"warning":      "该内容等同于账号登录态（含 refresh_token 与 DPoP 私钥），请勿外传或提交到版本库",
		})

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
		s.persist()
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
		s.persist()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": k.Key})
	case http.MethodDelete:
		q := r.URL.Query()
		if ref := q.Get("key"); ref != "" {
			_ = s.Store.DeleteKey(s.resolveKeyRef(ref))
		}
		if ref := q.Get("toggle"); ref != "" {
			_ = s.Store.ToggleKey(s.resolveKeyRef(ref))
		}
		s.persist()
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
		ClearPassword     *bool             `json:"clear_admin_password"`
		AutoRefresh       *bool             `json:"auto_refresh"`
		AllowCreditModels *bool             `json:"allow_credit_models"`
		AllowAnonymous    *bool             `json:"allow_anonymous"`
		RateLimitEnabled  *bool             `json:"rate_limit_enabled"`
		RateLimitQPS      *int              `json:"rate_limit_qps"`
		RateLimitBurst    *int              `json:"rate_limit_burst"`
		RateLimitConc     *int              `json:"rate_limit_concurrency"`
		WriteBackToIDE    *bool             `json:"write_back_to_ide"`
		LiveTTLSeconds    *int              `json:"live_ttl_sec"`
		UserDataDir       *string           `json:"user_data_dir"`
		LoginPort         *int              `json:"login_port"`
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
		setIf(&st.AllowAnonymous, body.AllowAnonymous)
		setIf(&st.RateLimitEnabled, body.RateLimitEnabled)
		setIf(&st.RateLimitQPS, body.RateLimitQPS)
		setIf(&st.RateLimitBurst, body.RateLimitBurst)
		setIf(&st.RateLimitConcurrency, body.RateLimitConc)
		setIf(&st.WriteBackToIDE, body.WriteBackToIDE)
		setIf(&st.LiveTTLSeconds, body.LiveTTLSeconds)
		setIf(&st.UserDataDir, body.UserDataDir)
		setIf(&st.LoginPort, body.LoginPort)
		setIf(&st.PluginName, body.PluginName)
		setIf(&st.PluginVersion, body.PluginVersion)
		setIf(&st.IDEName, body.IDEName)
		setIf(&st.IDEVersion, body.IDEVersion)
		setIf(&st.Language, body.Language)
		// 只有显式带 clear_admin_password 才清除：空字符串一律视为「不修改」。
		// 否则误发一个空字段就会把管理密码抹掉，让管理接口退回「本机直连」兜底。
		if body.ClearPassword != nil && *body.ClearPassword {
			st.AdminPassword = ""
		} else if body.AdminPassword != nil {
			if v := strings.TrimSpace(*body.AdminPassword); v != "" {
				st.AdminPassword = v
			}
		}
	})
	s.persist()
	// 身份头变更后重建上游客户端
	s.ApplyIdentity()
	// 限流参数变更后立即生效（不用重启）
	s.ApplyRateLimit()
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
			probe, ok := s.defaultProbeModel()
			if !ok {
				err = &apierr.ModelNotFound{Name: s.Store.Settings().DefaultModel}
			} else {
				err = s.Up.Probe(ctx, sess, probe.ID)
			}
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
	s.persist()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
}

// defaultProbeModel 返回探活用的模型。
//
// 探活会消耗极少量积分（max_tokens=8），控制台上已标注。
func (s *Server) defaultProbeModel() (registry.Model, bool) {
	st := s.Store.Settings()
	return s.Reg.Resolve(st.DefaultModel, st.ModelAliases)
}

// probeMaxModels 是一次测活请求允许探测的模型数上限。
//
// 每个模型都是一次真实上游请求、真实扣积分，因此必须有上限：上游清单异常变大时，
// 不能让一次点击变成无界的消耗。超限要求调用方分批，而不是静默截断。
const probeMaxModels = 64

// probeAttemptTimeout 是「一个模型 × 一个账号」的探测超时。
//
// 上游正常几百毫秒到几秒就返回；30 秒还没回基本可判定这次尝试无望，
// 早点换下一个账号，避免「多账号 × 多模型」把整个请求拖到客户端超时。
const probeAttemptTimeout = 30 * time.Second

// handleAdminModelProbe 逐个模型做一次最小真实请求，返回每个模型是否可用。
//
// 与 handleAdminCheckin 的区别：那个只探一个默认模型、判断的是「账号通不通」；
// 这个遍历当前模型清单，判断的是「这个账号能不能用某个模型」——上游会把当前
// 套餐未注册的模型也列进 /v1/models，只有真打一次才知道能不能用。
//
// 成本约束：一次探测 = 一次真实上游请求（max_tokens=8，真实扣积分），因此有三道闸门：
// 单次请求的模型数上限、同一时刻只允许一个测活任务、每次尝试的超时。
// 前端在全量测活前还要用户二次确认。
func (s *Server) handleAdminModelProbe(w http.ResponseWriter, r *http.Request) {
	// 同一时刻只允许一个测活任务：两个并发的全量探测会把成本翻倍，也会把单账号
	// 3 个并发会话的上限打满 —— 结果反而测出「不可用」，把用户引到错误结论上。
	if !s.probeMu.TryLock() {
		writeErr(w, http.StatusConflict, "已有测活任务在运行，请等它结束后再试", "invalid_request_error")
		return
	}
	defer s.probeMu.Unlock()

	var body struct {
		ID     string   `json:"id"`
		Models []string `json:"models"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	targets := s.Store.Accounts()
	if body.ID != "" {
		targets = nil
		if a := s.Store.Account(body.ID); a != nil {
			targets = append(targets, a)
		}
	}
	if len(targets) == 0 {
		writeErr(w, http.StatusBadRequest, "没有可用账号", "invalid_request_error")
		return
	}

	// 未指定模型时探测当前清单里的全部模型。
	want := body.Models
	if len(want) == 0 {
		st := s.Store.Settings()
		for _, m := range s.Reg.List(st.ModelAliases) {
			// 别名与本体重复探测没有意义，只探上游真实 ID。
			if m.Source == "alias" {
				continue
			}
			want = append(want, m.ID)
		}
	}
	want = dedupeModels(want)
	if len(want) == 0 {
		writeErr(w, http.StatusBadRequest, "模型清单为空", "invalid_request_error")
		return
	}
	if len(want) > probeMaxModels {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("一次最多测活 %d 个模型（当前 %d 个），请分批进行", probeMaxModels, len(want)),
			"invalid_request_error")
		return
	}

	// 并发上限按模型数收敛：每个探测都是一次真实上游请求，串行太慢、
	// 全并发又会撞单账号 3 会话的限制。
	limit := 4
	results := make([]map[string]any, len(want))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, modelID := range want {
		wg.Add(1)
		go func(idx int, modelID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			row := map[string]any{"model": modelID}
			// 依次尝试账号，任一账号能用即判定该模型可用。
			for _, a := range targets {
				// 客户端断开后不必再花积分试下一个账号。
				if r.Context().Err() != nil {
					row["ok"] = false
					row["error"] = "测活已取消"
					break
				}
				ctx, cancel := context.WithTimeout(r.Context(), probeAttemptTimeout)
				sess, err := s.Cred.Session(ctx, a, s.resolveOptions())
				if err != nil {
					row["ok"] = false
					row["error"] = err.Error()
					row["account"] = a.DisplayName()
					cancel()
					continue
				}
				err = s.Up.Probe(ctx, sess, modelID)
				cancel()
				if err == nil {
					row["ok"] = true
					row["account"] = a.DisplayName()
					break
				}
				row["ok"] = false
				row["error"] = err.Error()
				row["account"] = a.DisplayName()
			}
			results[idx] = row
		}(i, modelID)
	}
	wg.Wait()

	okCount := 0
	for _, row := range results {
		if row["ok"] == true {
			okCount++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "total": len(results), "available": okCount,
		"unavailable": len(results) - okCount, "results": results,
	})
}

// dedupeModels 去掉空值、首尾空白与重复的模型 ID。
//
// 重复 ID 会变成两次真实扣费，而调用方（控制台）没有任何理由需要重复探测。
func dedupeModels(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// creditCacheTTL 是积分额度的缓存时长。
//
// 额度查询是一次真实上游请求；切换页签 / 连续刷新时没必要每次都打上游。
// 控制台点「刷新额度」会带 ?fresh=1 绕过缓存。
const creditCacheTTL = 60 * time.Second

// creditCacheEntry 是一次额度查询的结果。失败也缓存，避免上游故障时被反复重试。
type creditCacheEntry struct {
	info *codearts.CreditInfo
	err  error
	at   time.Time
}

// handleAdminCredits 返回账号的积分额度（上游 snap-manager 统计插件）。
//
//	GET /admin/api/credits           → 全部账号
//	GET /admin/api/credits?id=<账号>  → 指定账号
//	GET ...?fresh=1                  → 跳过 60 秒缓存
func (s *Server) handleAdminCredits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET", "invalid_request_error")
		return
	}
	fresh := r.URL.Query().Get("fresh") != ""
	targets := s.Store.Accounts()
	if id := strings.TrimSpace(r.URL.Query().Get("id")); id != "" {
		targets = nil
		if a := s.Store.Account(id); a != nil {
			targets = append(targets, a)
		}
	}
	if len(targets) == 0 {
		writeErr(w, http.StatusBadRequest, apierr.NoAccount.Error(), "invalid_request_error")
		return
	}

	// 每个账号一次上游请求：并发 4 路，避免把单账号 3 个并发会话的上限打满。
	type creditRow struct {
		idx int
		row map[string]any
	}
	results := make([]map[string]any, len(targets))
	ch := make(chan creditRow, len(targets))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, a := range targets {
		wg.Add(1)
		go func(idx int, a *codearts.Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			row := map[string]any{"id": a.ID, "name": a.DisplayName()}
			info, err := s.creditFor(r.Context(), a, fresh)
			if err != nil {
				row["ok"] = false
				row["error"] = err.Error()
			} else {
				row["ok"] = true
				row["credit"] = info
			}
			// 只往 channel 写，切片由主 goroutine 汇总，避免并发写。
			ch <- creditRow{idx: idx, row: row}
		}(i, a)
	}
	wg.Wait()
	close(ch)
	for c := range ch {
		results[c.idx] = c.row
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": results})
}

// creditFor 解析凭据并查询积分额度，带短缓存。
func (s *Server) creditFor(ctx context.Context, a *codearts.Account, fresh bool) (*codearts.CreditInfo, error) {
	if !fresh {
		if info, err, ok := s.cachedCredit(a.ID); ok {
			return info, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	sess, err := s.Cred.Session(ctx, a, s.resolveOptions())
	if err != nil {
		s.storeCredit(a.ID, nil, err)
		return nil, err
	}
	info, err := s.Up.FetchCreditInfo(ctx, sess)
	s.storeCredit(a.ID, info, err)
	return info, err
}

// cachedCredit 返回 TTL 内的缓存结果。
func (s *Server) cachedCredit(id string) (*codearts.CreditInfo, error, bool) {
	s.creditMu.Lock()
	defer s.creditMu.Unlock()
	e, ok := s.creditCache[id]
	if !ok || time.Since(e.at) > creditCacheTTL {
		return nil, nil, false
	}
	return e.info, e.err, true
}

// storeCredit 记录一次额度查询结果。
func (s *Server) storeCredit(id string, info *codearts.CreditInfo, err error) {
	s.creditMu.Lock()
	defer s.creditMu.Unlock()
	if s.creditCache == nil {
		s.creditCache = map[string]creditCacheEntry{}
	}
	s.creditCache[id] = creditCacheEntry{info: info, err: err, at: time.Now()}
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
