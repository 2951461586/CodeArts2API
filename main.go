// CodeArts2API 把本机 CodeArts Agent（华为云码道 / CodeArts Doer for Coding）
// 桌面端的上游模型服务，逆向复刻成一个 OpenAI 兼容的 Go 网关。
//
// 复用桌面端已登录的 STS 临时凭据（DPAPI + AES-GCM 解出），
// 用华为云 SDK-HMAC-SHA256 签名直连 inferhub；纯 Go 标准库实现。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"codearts2api/internal/codearts"
	"codearts2api/internal/server"
	"codearts2api/internal/store"
)

// version 由构建时注入：-ldflags "-X main.version=1.0.0"
var version = "dev"

func main() {
	var (
		host       = flag.String("host", envOr("CODEARTS_HOST", "127.0.0.1"), "监听地址")
		port       = flag.Int("port", envInt("CODEARTS_PORT", 10087), "监听端口")
		dataPath   = flag.String("data", envOr("CODEARTS_DATA_PATH", ""), "状态文件路径（默认在可执行文件同目录）")
		userData   = flag.String("user-data", envOr("CODEARTS_USER_DATA", ""), "CodeArts Agent 的 User Data 目录（留空自动探测）")
		importOnly = flag.Bool("import", false, "仅执行一次桌面端凭据导入后退出")
		noImport   = flag.Bool("no-import", false, "启动时不自动导入桌面端登录态")
		noRefresh  = flag.Bool("no-refresh", false, "禁止网关自行续期（只读 IDE 会话）")
		adminPw    = flag.String("admin-password", envOr("CODEARTS_ADMIN_PASSWORD", ""), "管理密码（非空时覆盖状态文件中的设置）")
		showVer    = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("codearts2api", version)
		return
	}

	if *dataPath == "" {
		*dataPath = defaultDataPath()
	}

	st, err := store.Open(*dataPath)
	if err != nil {
		log.Fatalf("打开状态文件失败: %v", err)
	}
	log.Printf("状态文件: %s", st.Path())

	if *importOnly {
		runImport(st, *userData)
		return
	}

	// 命令行 / 环境变量优先级高于状态文件；设置后立即落盘。
	if *adminPw != "" {
		st.UpdateSettings(func(s *store.Settings) { s.AdminPassword = *adminPw })
		if err := st.Save(); err != nil {
			log.Printf("保存管理密码失败: %v", err)
		} else {
			log.Printf("已启用管理密码（来自命令行 / CODEARTS_ADMIN_PASSWORD）")
		}
	}
	if *userData != "" {
		st.UpdateSettings(func(s *store.Settings) { s.UserDataDir = *userData })
	}
	if *noRefresh {
		st.UpdateSettings(func(s *store.Settings) {
			s.AutoRefresh = false
			s.WriteBackToIDE = false
		})
		log.Printf("已关闭网关自行续期，仅读取 IDE 会话")
	}

	if !*noImport {
		autoImport(st, *userData)
	}

	srv := server.New(st)
	srv.ApplyIdentity()

	// 后台刷新一次模型清单（不阻塞启动）
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := srv.RefreshModels(ctx); err != nil {
			log.Printf("刷新上游模型清单失败（将使用内置兜底清单）: %v", err)
		} else {
			log.Printf("已同步上游模型清单")
		}
	}()

	addr := net.JoinHostPort(*host, fmt.Sprintf("%d", *port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
		// 流式响应可能持续数分钟，不设 WriteTimeout
	}

	go func() {
		log.Printf("CodeArts2API %s 已启动: http://%s/", version, addr)
		if st.HasKeys() {
			log.Printf("已签发 API 密钥，调用需携带 Authorization: Bearer sk-ca-...")
		} else {
			log.Printf("尚未签发 API 密钥，当前允许无密钥调用（控制台可创建）")
		}
		if st.Settings().AdminPassword != "" {
			log.Printf("管理接口已启用密码校验（X-Admin-Password / Bearer）")
		} else {
			log.Printf("未设置管理密码，管理接口仅允许本机访问（可用 -admin-password 设置）")
		}
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("监听失败: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Printf("正在关闭…")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("关闭异常: %v", err)
	}
	if err := st.Save(); err != nil {
		log.Printf("保存状态失败: %v", err)
	}
	log.Printf("已退出")
}

// autoImport 在启动时尝试导入本机桌面端登录态。
func autoImport(st *store.Store, dir string) {
	results, err := codearts.ImportFromDesktop(dir)
	if err != nil {
		log.Printf("未自动导入桌面端登录态（可在控制台手动导入）: %v", err)
		return
	}
	added, updated := 0, 0
	for _, res := range results {
		if st.UpsertAccount(res.Account) {
			added++
		} else {
			updated++
		}
	}
	if err := st.Save(); err != nil {
		log.Printf("保存账号失败: %v", err)
		return
	}
	for _, res := range results {
		log.Printf("已导入账号 %s（目录=%s，AK=%s，有效期至 %s）",
			res.Account.Name, res.Dir, codearts.Mask(res.AK), res.Expires.Local().Format("2006-01-02 15:04:05"))
	}
	log.Printf("桌面端登录态导入完成：新增 %d，更新 %d", added, updated)
}

// runImport 只做导入并打印结果。
func runImport(st *store.Store, dir string) {
	results, err := codearts.ImportFromDesktop(dir)
	if err != nil {
		log.Fatalf("导入失败: %v", err)
	}
	added, updated := 0, 0
	for _, res := range results {
		isNew := st.UpsertAccount(res.Account)
		state := "更新"
		if isNew {
			state = "新增"
			added++
		} else {
			updated++
		}
		fmt.Printf("%s账号 %s\nUser Data: %s\nAccess Key: %s\n有效期至: %s\n可续期: %v\n\n",
			state, res.Account.Name, res.Dir, codearts.Mask(res.AK),
			res.Expires.Local().Format("2006-01-02 15:04:05"), res.Account.Session.HasRefresh())
	}
	if err := st.Save(); err != nil {
		log.Fatalf("保存失败: %v", err)
	}
	fmt.Printf("共导入 %d 个账号（新增 %d，更新 %d）→ %s\n", len(results), added, updated, st.Path())
}

// defaultDataPath 返回默认状态文件路径（可执行文件同目录）。
func defaultDataPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "codearts2api-data.json"
	}
	return filepath.Join(filepath.Dir(exe), "codearts2api-data.json")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}
