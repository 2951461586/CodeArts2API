// Package store 负责网关状态的持久化与账号池调度。
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"codearts2api/internal/codearts"
)

// APIKey 是网关对外签发的调用密钥。
type APIKey struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	LastUsed  int64  `json:"last_used"`
	Requests  int64  `json:"requests"`
}

// Settings 是运行期可调参数。
type Settings struct {
	ModelAliases   map[string]string `json:"model_aliases"`
	DefaultModel   string            `json:"default_model"`
	ForceModel     string            `json:"force_model"`
	RequestTimeout int               `json:"request_timeout_sec"`
	MaxRetries     int               `json:"max_retries"`
	LogKeep        int               `json:"log_keep"`
	CORSOrigin     string            `json:"cors_origin"`
	AdminPassword  string            `json:"admin_password"`

	// AutoRefresh 允许在凭据过期且 IDE 未续期时自行用 refresh_token 续期。
	AutoRefresh bool `json:"auto_refresh"`
	// AllowCreditModels 允许调用走积分（计费）通道的模型，如 GLM-5.3。
	// 关闭后只放行免费权益通道的模型。
	AllowCreditModels bool `json:"allow_credit_models"`
	// AllowAnonymous 允许在尚未签发任何 API 密钥时，从非本机来源匿名调用 /v1/*。
	//
	// 默认关闭：没密钥时只放行本机直连。置真等于把账号额度开放给能访问到
	// 这个端口的所有人，仅在你确定端口没有对外暴露时才开启。
	AllowAnonymous bool `json:"allow_anonymous"`
	// AutoClaim 每天自动领取免费福利 Token（幂等，重复领不报错）。
	AutoClaim bool `json:"auto_claim"`
	// ClaimAfterHour / ClaimAfterMinute 是每天开始签到的本地时刻。
	// 福利 Token 当日 24:00 清零，所以默认 00:05 领。
	ClaimAfterHour   int `json:"claim_after_hour"`
	ClaimAfterMinute int `json:"claim_after_minute"`
	// WriteBackToIDE 允许把续期结果回写 state.vscdb。
	WriteBackToIDE bool `json:"write_back_to_ide"`
	// LiveTTLSeconds 是重读 IDE 会话的缓存时长。
	LiveTTLSeconds int `json:"live_ttl_sec"`
	// UserDataDir 是 CodeArts Agent 的 User Data 目录（留空自动探测）。
	UserDataDir string `json:"user_data_dir"`
	// LoginPort 固定浏览器登录的回调端口（0 = 随机 ≥10000）。
	//
	// 远端部署时固定它，就能配一条常驻 SSH 本地转发：
	//     ssh -N -L <LoginPort>:127.0.0.1:<LoginPort> user@server
	LoginPort int `json:"login_port"`

	// 上游身份头（留空用与 26.9.902 对齐的缺省值）
	PluginName    string `json:"plugin_name"`
	PluginVersion string `json:"plugin_version"`
	IDEName       string `json:"ide_name"`
	IDEVersion    string `json:"ide_version"`
	Language      string `json:"language"`
}

// LogEntry 是一条请求日志（只记元信息，不落对话内容）。
type LogEntry struct {
	Time      int64  `json:"time"`
	Key       string `json:"key"`
	Account   string `json:"account"`
	Model     string `json:"model"`
	Stream    bool   `json:"stream"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	PromptLen int    `json:"prompt_len"`
	OutputLen int    `json:"output_len"`
	Err       string `json:"err,omitempty"`
}

// Stats 是累计统计。
type Stats struct {
	TotalRequests  int64 `json:"total_requests"`
	FailedRequests int64 `json:"failed_requests"`
	TotalCharsOut  int64 `json:"total_chars_out"`
	StartedAt      int64 `json:"started_at"`
}

// State 是全部持久化状态。
type State struct {
	Accounts []*codearts.Account `json:"accounts"`
	Keys     []*APIKey           `json:"keys"`
	Settings Settings            `json:"settings"`
	Stats    Stats               `json:"stats"`
	Logs     []LogEntry          `json:"logs"`
}

// Store 是带锁的状态容器。
type Store struct {
	// mu 保护 st 里的字段（含账号指针指向的对象）。
	mu sync.RWMutex
	// saveMu 串行化落盘。
	//
	// 并发 Save 曾共用同一个 `<path>.tmp`：Windows 上直接报「文件被占用」
	// 而失败（调用方多半忽略错误 → 状态静默不落盘），Linux 上则可能把写了一半
	// 的临时文件 rename 成正式状态文件（含 refresh_token 的账号全部丢失）。
	// 因此落盘必须串行，并且临时文件名要唯一。
	saveMu sync.Mutex
	path   string
	st     *State
	rotate int
}

// DefaultSettings 返回默认设置。
func DefaultSettings() Settings {
	return Settings{
		ModelAliases:   map[string]string{},
		DefaultModel:   "glm-5.3-flash",
		RequestTimeout: 300,
		MaxRetries:     3,
		LogKeep:        500,
		// 默认仅放行本机来源；如需局域网访问，在控制台显式配置。
		CORSOrigin:        "",
		AutoRefresh:       true,
		AllowCreditModels: true,
		AutoClaim:         true,
		ClaimAfterHour:    0,
		ClaimAfterMinute:  5,
		WriteBackToIDE:    true,
		LiveTTLSeconds:    10,
		PluginName:        codearts.PluginName,
		PluginVersion:     codearts.PluginVersion,
		IDEName:           codearts.IDEName,
		IDEVersion:        codearts.IDEVersion,
		Language:          "zh-cn",
	}
}

// Open 载入（或初始化）状态文件。
func Open(path string) (*Store, error) {
	s := &Store{path: path, st: &State{Settings: DefaultSettings()}}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s.st); err != nil {
			return nil, fmt.Errorf("状态文件解析失败: %w", err)
		}
		s.applyDefaults()
	case errors.Is(err, os.ErrNotExist):
		s.st.Stats.StartedAt = time.Now().Unix()
	default:
		return nil, err
	}
	return s, nil
}

// applyDefaults 补齐缺失 / 非法字段。
func (s *Store) applyDefaults() {
	def := DefaultSettings()
	st := &s.st.Settings
	if st.ModelAliases == nil {
		st.ModelAliases = def.ModelAliases
	}
	if st.DefaultModel == "" {
		st.DefaultModel = def.DefaultModel
	}
	if st.RequestTimeout <= 0 {
		st.RequestTimeout = def.RequestTimeout
	}
	if st.MaxRetries <= 0 {
		st.MaxRetries = def.MaxRetries
	}
	if st.LogKeep <= 0 {
		st.LogKeep = def.LogKeep
	}
	if st.LiveTTLSeconds <= 0 {
		st.LiveTTLSeconds = def.LiveTTLSeconds
	}
	if st.ClaimAfterHour < 0 || st.ClaimAfterHour > 23 {
		st.ClaimAfterHour = def.ClaimAfterHour
	}
	if st.ClaimAfterMinute < 0 || st.ClaimAfterMinute > 59 {
		st.ClaimAfterMinute = def.ClaimAfterMinute
	}
	// 回调端口要么不固定（0），要么落在门户要求的区间内
	if st.LoginPort != 0 && (st.LoginPort < 10000 || st.LoginPort > 65535) {
		st.LoginPort = 0
	}
	if st.PluginName == "" {
		st.PluginName = def.PluginName
	}
	if st.PluginVersion == "" {
		st.PluginVersion = def.PluginVersion
	}
	if st.IDEName == "" {
		st.IDEName = def.IDEName
	}
	if st.IDEVersion == "" {
		st.IDEVersion = def.IDEVersion
	}
	if st.Language == "" {
		st.Language = def.Language
	}
	if st.CORSOrigin == "*" {
		// 历史状态文件里的通配符不再生效，收紧为本机来源。
		st.CORSOrigin = ""
	}
	if s.st.Stats.StartedAt == 0 {
		s.st.Stats.StartedAt = time.Now().Unix()
	}
}

// Path 返回状态文件路径。
func (s *Store) Path() string { return s.path }

// Save 原子写回状态文件。
//
// 并发安全：saveMu 保证同一时刻只有一个 goroutine 在写；临时文件名带随机后缀，
// 即使有第二个进程共享同一个状态文件也不会互相截断。
func (s *Store) Save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	raw, err := json.MarshalIndent(s.st, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// os.CreateTemp 以 0600 创建 —— 状态文件里含 STS 凭据与 refresh_token。
	f, err := os.CreateTemp(dir, filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // rename 成功后这里是 no-op

	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// cloneAccount 返回账号的浅拷贝。
//
// 对外只暴露拷贝：Account 的字段（Enabled / Status / Failures / LastErr / Session…）
// 会被请求路径（MarkAccountResult）与控制台并发读写，直接把内部指针交出去
// 就是数据竞争。Session 指针可以共享 —— 会话构造完成后不再原地修改，
// 续期时是整体替换指针。
func cloneAccount(a *codearts.Account) *codearts.Account {
	if a == nil {
		return nil
	}
	cp := *a
	return &cp
}

// cloneAccounts 批量浅拷贝。
func cloneAccounts(in []*codearts.Account) []*codearts.Account {
	out := make([]*codearts.Account, len(in))
	for i, a := range in {
		out[i] = cloneAccount(a)
	}
	return out
}

// --- 账号 ---

// Accounts 返回账号快照（元素是浅拷贝，可直接并发读取）。
func (s *Store) Accounts() []*codearts.Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneAccounts(s.st.Accounts)
}

// Account 按 ID 查找账号，返回快照。
func (s *Store) Account(id string) *codearts.Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.st.Accounts {
		if a.ID == id {
			return cloneAccount(a)
		}
	}
	return nil
}

// UpsertAccount 按 UserData / AK 去重后写入账号。
//
// 入库时拷贝一份：store 拥有自己的副本，调用方之后对入参的修改不会绕过锁
// 偷偷改到 store 内部状态。
func (s *Store) UpsertAccount(a *codearts.Account) bool {
	if a == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, ex := range s.st.Accounts {
		sameDir := ex.UserData != "" && ex.UserData == a.UserData
		sameAK := ex.Session != nil && a.Session != nil &&
			ex.Session.AccessKey != "" && ex.Session.AccessKey == a.Session.AccessKey
		if sameDir || sameAK {
			a.ID = ex.ID
			s.st.Accounts[i] = cloneAccount(a)
			return false
		}
	}
	s.st.Accounts = append(s.st.Accounts, cloneAccount(a))
	return true
}

// RemoveAccount 删除账号。
func (s *Store) RemoveAccount(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.st.Accounts {
		if a.ID == id {
			s.st.Accounts = append(s.st.Accounts[:i], s.st.Accounts[i+1:]...)
			return true
		}
	}
	return false
}

// NextAccount 以轮询方式挑一个可用账号，返回快照。
//
// 连续失败过多的账号会被跳过；全部不可用时回退到失败最少的那个，
// 保证不会硬阻塞请求。
func (s *Store) NextAccount() *codearts.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.st.Accounts)
	if n == 0 {
		return nil
	}
	var best *codearts.Account
	for i := 0; i < n; i++ {
		idx := (s.rotate + i) % n
		a := s.st.Accounts[idx]
		if !a.Enabled {
			continue
		}
		if best == nil || a.Failures < best.Failures {
			best = a
		}
		if a.Failures == 0 {
			s.rotate = (idx + 1) % n
			return cloneAccount(a)
		}
	}
	s.rotate = (s.rotate + 1) % n
	return cloneAccount(best)
}

// SetAccountEnabled 启用 / 停用账号（在锁内修改，避免与控制台读取竞争）。
func (s *Store) SetAccountEnabled(id string, enabled bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.st.Accounts {
		if a.ID == id {
			a.Enabled = enabled
			return true
		}
	}
	return false
}

// ClearAccountErrors 清零失败计数与错误状态。
func (s *Store) ClearAccountErrors(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.st.Accounts {
		if a.ID == id {
			a.Failures = 0
			a.Status = "ok"
			a.LastErr = ""
			return true
		}
	}
	return false
}

// SetAccountSession 用新的会话替换账号上缓存的凭据（续期 / 跟随 IDE 时调用）。
//
// 凭据解析走的是快照，所以 Manager 拿到新会话后必须通过这里写回，
// 否则续期结果只存在于那次请求里。
func (s *Store) SetAccountSession(id string, sess *codearts.Session) bool {
	if sess == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.st.Accounts {
		if a.ID == id {
			a.Session = sess
			return true
		}
	}
	return false
}

// MarkAccountResult 记录一次账号调用结果。
func (s *Store) MarkAccountResult(id string, ok bool, note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.st.Accounts {
		if a.ID != id {
			continue
		}
		a.LastUsed = time.Now().Unix()
		if ok {
			a.Failures = 0
			a.Status = "ok"
			a.LastErr = ""
			if note != "" {
				a.Note = note
			}
		} else {
			a.Failures++
			a.Status = "error"
			a.LastErr = note
		}
		return
	}
}

// MarkClaimResult 记录一次每日签到结果。
//
// ok 为真时记下日期（同一天不会重复签），失败时只记错误、不写日期，
// 这样下一轮（5 分钟后）还会重试。
func (s *Store) MarkClaimResult(id, date string, ok bool, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.st.Accounts {
		if a.ID != id {
			continue
		}
		a.LastClaimAt = time.Now().Unix()
		if ok {
			a.LastClaimDate = date
			a.LastClaimErr = ""
			a.LastClaimMsg = msg
		} else {
			a.LastClaimErr = msg
		}
		return
	}
}

// --- 密钥 ---

// Keys 返回密钥快照。
func (s *Store) Keys() []*APIKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*APIKey, len(s.st.Keys))
	copy(out, s.st.Keys)
	return out
}

// AddKey 新增密钥。
func (s *Store) AddKey(name string) *APIKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := &APIKey{
		Key:       "sk-ca-" + codearts.RandomHex(24),
		Name:      name,
		Enabled:   true,
		CreatedAt: time.Now().Unix(),
	}
	s.st.Keys = append(s.st.Keys, k)
	return k
}

// DeleteKey 删除密钥。
func (s *Store) DeleteKey(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.st.Keys {
		if k.Key == key {
			s.st.Keys = append(s.st.Keys[:i], s.st.Keys[i+1:]...)
			return true
		}
	}
	return false
}

// ToggleKey 启用 / 停用密钥。
func (s *Store) ToggleKey(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.st.Keys {
		if k.Key == key {
			k.Enabled = !k.Enabled
			return true
		}
	}
	return false
}

// ValidKey 校验密钥并计数。
func (s *Store) ValidKey(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.st.Keys {
		if k.Key == key && k.Enabled {
			k.LastUsed = time.Now().Unix()
			k.Requests++
			return true
		}
	}
	return false
}

// HasKeys 报告是否已签发任何密钥。
func (s *Store) HasKeys() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.st.Keys) > 0
}

// --- 设置 / 统计 / 日志 ---

// Settings 返回设置快照。
func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := s.st.Settings
	cp.ModelAliases = map[string]string{}
	for k, v := range s.st.Settings.ModelAliases {
		cp.ModelAliases[k] = v
	}
	return cp
}

// UpdateSettings 局部更新设置。
func (s *Store) UpdateSettings(fn func(*Settings)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st.Settings)
}

// Stats 返回统计快照。
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.Stats
}

// Logs 返回最近的日志。
func (s *Store) Logs(limit int) []LogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := len(s.st.Logs)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]LogEntry, limit)
	copy(out, s.st.Logs[n-limit:])
	return out
}

// Record 记录一次请求结果并更新统计。
func (s *Store) Record(e LogEntry, outChars int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Stats.TotalRequests++
	if e.Status >= 400 || e.Err != "" {
		s.st.Stats.FailedRequests++
	}
	s.st.Stats.TotalCharsOut += int64(outChars)
	s.st.Logs = append(s.st.Logs, e)
	if keep := s.st.Settings.LogKeep; keep > 0 && len(s.st.Logs) > keep {
		s.st.Logs = s.st.Logs[len(s.st.Logs)-keep:]
	}
}

// ClearLogs 清空日志。
func (s *Store) ClearLogs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Logs = nil
}

// ClearStats 重置统计。
func (s *Store) ClearStats() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Stats = Stats{StartedAt: time.Now().Unix()}
}
