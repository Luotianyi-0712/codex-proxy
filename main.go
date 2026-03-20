/**
 * Codex Proxy 独立服务入口
 * 提供 OpenAI 兼容的 API 接口，将请求转发至 Codex (OpenAI Responses API)
 * 支持多账号轮询、Token 自动刷新、思考配置（连字符格式）
 */
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"codex-proxy/internal/auth"
	"codex-proxy/internal/config"
	"codex-proxy/internal/executor"
	"codex-proxy/internal/handler"
	"codex-proxy/internal/static"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

/* ANSI 颜色代码 */
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorCyan   = "\033[36m"
	colorGray   = "\033[90m"
	colorWhite  = "\033[97m"
)

func openOrCreatePostgres(cfg *config.Config) (*sql.DB, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}

	targetName := strings.TrimSpace(cfg.DBName)
	if targetName == "" {
		targetName = "codex_proxy"
	}

	dsn := strings.TrimSpace(cfg.DBDSN)
	if dsn == "" {
		dsn = fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
			cfg.DBUser, cfg.DBPassword, cfg.DBHost, cfg.DBPort, targetName, cfg.DBSSLMode)
	}

	openDB := func(uri string) (*sql.DB, error) {
		db, err := sql.Open(cfg.DBDriver, uri)
		if err != nil {
			return nil, err
		}
		if err = db.Ping(); err != nil {
			_ = db.Close()
			return nil, err
		}
		return db, nil
	}

	db, err := openDB(dsn)
	if err == nil {
		return db, nil
	}

	if !strings.Contains(err.Error(), "does not exist") && !strings.Contains(err.Error(), "不存在") {
		return nil, err
	}

	// 尝试自动创建数据库
	adminDSN := dsn
	if cfg.DBDSN == "" {
		adminDSN = fmt.Sprintf("postgres://%s:%s@%s:%d/postgres?sslmode=%s",
			cfg.DBUser, cfg.DBPassword, cfg.DBHost, cfg.DBPort, cfg.DBSSLMode)
	} else {
		parsed, parseErr := url.Parse(cfg.DBDSN)
		if parseErr == nil {
			parsed.Path = "/postgres"
			adminDSN = parsed.String()
		}
	}

	adminDB, err := openDB(adminDSN)
	if err != nil {
		return nil, fmt.Errorf("admin DB 连接失败: %w", err)
	}
	defer adminDB.Close()

	_, err = adminDB.Exec(fmt.Sprintf("CREATE DATABASE %s", pqQuote(targetName)))
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return nil, fmt.Errorf("创建数据库失败: %w", err)
	}

	// 重连目标数据库
	return openDB(dsn)
}

func pqQuote(identifier string) string {
	identifier = strings.ReplaceAll(identifier, "\"", "\\\"")
	return "\"" + identifier + "\""
}
func main() {
	/* 配置 logrus 彩色日志格式 */
	log.SetFormatter(&log.TextFormatter{
		ForceColors:     true,
		FullTimestamp:   true,
		TimestampFormat: "15:04:05",
	})

	configPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	/* 加载配置 */
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	log.Infof("%s⚡ Codex Proxy 启动中...%s", colorCyan, colorReset)
	log.Infof("监听地址: %s%s%s", colorGreen, cfg.Listen, colorReset)
	log.Infof("账号目录: %s", cfg.AuthDir)
	log.Infof("API 基础 URL: %s", cfg.BaseURL)
	log.Infof("刷新间隔: %d 秒", cfg.RefreshInterval)
	log.Infof("最大重试: %d 次", cfg.MaxRetry)
	if cfg.HealthCheckInterval > 0 {
		log.Infof("健康检查: 每 %d 秒, 并发 %d, 连续失败 %d 次禁用",
			cfg.HealthCheckInterval, cfg.HealthCheckConcurrency, cfg.HealthCheckMaxFailures)
	}

	/* 数据库连接（可选） */
	var db *sql.DB
	if cfg.DBEnabled {
		db, err = openOrCreatePostgres(cfg)
		if err != nil {
			log.Fatalf("数据库无法就绪: %v", err)
		}
		log.Infof("已连接数据库")

		if err = auth.SetupDB(db); err != nil {
			log.Fatalf("数据库初始化失败: %v", err)
		}
	}

	/* 初始化账号管理器 */
	var selector auth.Selector
	if cfg.Selector == "quota-first" {
		selector = auth.NewQuotaFirstSelector()
	} else {
		selector = auth.NewRoundRobinSelector()
	}
	managerOpts := &auth.ManagerOptions{
		AuthScanInterval:        cfg.AuthScanInterval,
		SaveWorkers:             cfg.SaveWorkers,
		Cooldown401Sec:          cfg.Cooldown401Sec,
		Cooldown429Sec:          cfg.Cooldown429Sec,
		RefreshSingleTimeoutSec: cfg.RefreshSingleTimeoutSec,
		RefreshBatchSize:        cfg.RefreshBatchSize,
	}
	manager := auth.NewManager(cfg.AuthDir, db, cfg.ProxyURL, cfg.RefreshInterval, selector, cfg.EnableHTTP2, managerOpts)
	manager.SetRefreshConcurrency(cfg.RefreshConcurrency)

	/* 启动后台任务 */
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if cfg.StartupAsyncLoad {
		log.Infof("启动即服务可用: 已启用后台账号加载模式")
		go func() {
			start := time.Now()
			for {
				if ctx.Err() != nil {
					return
				}
				if loadErr := manager.LoadAccounts(); loadErr != nil {
					retrySec := cfg.StartupLoadRetryInterval
					if retrySec < 1 {
						retrySec = 10
					}
					log.Warnf("后台加载账号失败: %v，%d 秒后重试", loadErr, retrySec)
					select {
					case <-ctx.Done():
						return
					case <-time.After(time.Duration(retrySec) * time.Second):
					}
					continue
				}
				log.Infof("后台加载账号完成: 共 %d 个，耗时 %v", manager.AccountCount(), time.Since(start).Round(time.Millisecond))
				return
			}
		}()
	} else {
		loadStart := time.Now()
		if err = manager.LoadAccounts(); err != nil {
			log.Fatalf("加载账号失败: %v", err)
		}
		log.Infof("账号加载完成: 共 %d 个，耗时 %v", manager.AccountCount(), time.Since(loadStart).Round(time.Millisecond))
	}

	/* 启动异步磁盘写入工作器（将 Token 写盘从刷新 goroutine 解耦） */
	manager.StartSaveWorker(ctx)

	/* 启动后台 Token 刷新 */
	go manager.StartRefreshLoop(ctx)

	/* 启动健康检查（如果配置了检查间隔） */
	if cfg.HealthCheckInterval > 0 {
		healthChecker := auth.NewHealthChecker(
			cfg.BaseURL, cfg.ProxyURL,
			cfg.HealthCheckInterval,
			cfg.HealthCheckMaxFailures,
			cfg.HealthCheckConcurrency,
			cfg.HealthCheckStartDelay,
			cfg.HealthCheckBatchSize,
			cfg.HealthCheckReqTimeout,
			cfg.EnableHTTP2,
			cfg.BackendDomain,
			cfg.BackendResolveAddress,
		)
		go healthChecker.StartLoop(ctx, manager)
	}

	/* 初始化执行器 */
	exec := executor.NewExecutor(cfg.BaseURL, cfg.ProxyURL, executor.HTTPPoolConfig{
		MaxConnsPerHost:      cfg.MaxConnsPerHost,
		MaxIdleConns:         cfg.MaxIdleConns,
		MaxIdleConnsPerHost:  cfg.MaxIdleConnsPerHost,
		EnableHTTP2:          cfg.EnableHTTP2,
		BackendDomain:        cfg.BackendDomain,
		ResolveAddress:       cfg.BackendResolveAddress,
		KeepaliveIntervalSec: cfg.KeepaliveInterval,
	})

	/* 启动连接池保活（防止长时间无请求后首次请求耗时过长） */
	exec.StartKeepAlive(ctx)

	/* 初始化 HTTP 服务 */
	if cfg.LogLevel != "debug" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(handler.CORSAllowOrigin())
	r.Use(handler.GzipIfAccepted())
	r.Use(handler.OptionsBypass())
	r.Use(gin.Recovery())
	r.Use(ginLogger())

	/* 注册路由 */
	proxyHandler := handler.NewProxyHandler(manager, exec, cfg.APIKeys, cfg.MaxRetry, cfg.ProxyURL, cfg.BaseURL, cfg.EnableHTTP2, cfg.BackendDomain, cfg.BackendResolveAddress, cfg.QuotaCheckConcurrency, cfg.UpstreamTimeoutSec, cfg.EmptyRetryMax, cfg.StreamIdleTimeoutSec, cfg.EnableStreamIdleRetry, static.IndexHTML)
	proxyHandler.RegisterRoutes(r)

	/* HTTP/2 明文 (h2c)：单连接多路复用；未升级的客户端仍走 HTTP/1.1 */
	h2s := &http2.Server{}
	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: h2c.NewHandler(r, h2s),
	}

	/* 在 goroutine 中启动 HTTP 服务 */
	go func() {
		log.Infof("%s⚡ Codex Proxy 已启动%s，共 %s%d%s 个账号，监听 %s%s%s",
			colorCyan, colorReset,
			colorGreen, manager.AccountCount(), colorReset,
			colorGreen, cfg.Listen, colorReset)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务启动失败: %v", err)
		}
	}()

	/* 等待关闭信号 */
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Infof("%s收到关闭信号，正在停止...%s", colorYellow, colorReset)

	/* 优雅关闭 HTTP 服务器 */
	shutdownSec := cfg.ShutdownTimeout
	if shutdownSec < 1 {
		shutdownSec = 5
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Duration(shutdownSec)*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Errorf("HTTP 服务关闭异常: %v", err)
	}

	/* 停止后台任务 */
	cancel()
	manager.Stop()

	log.Infof("%s✅ Codex Proxy 已停止%s", colorGreen, colorReset)
}

/**
 * ginLogger 自定义 Gin 日志中间件（彩色输出）
 * @returns gin.HandlerFunc - Gin 中间件函数
 */
func ginLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		status := c.Writer.Status()
		latency := time.Since(start)
		method := c.Request.Method
		path := c.Request.URL.Path
		client := c.ClientIP()

		/* 状态码着色 */
		statusColor := colorGreen
		switch {
		case status >= 500:
			statusColor = colorRed
		case status >= 400:
			statusColor = colorYellow
		case status >= 300:
			statusColor = colorCyan
		}

		/* 方法着色 */
		methodColor := colorBlue
		switch method {
		case "POST":
			methodColor = colorCyan
		case "DELETE":
			methodColor = colorRed
		case "PUT", "PATCH":
			methodColor = colorYellow
		}

		log.Infof("%s%s%s %s%d%s %s%s%s %s%v%s %s",
			methodColor, method, colorReset,
			statusColor, status, colorReset,
			colorWhite, path, colorReset,
			colorGray, latency.Round(time.Millisecond), colorReset,
			fmt.Sprintf("%s%s%s", colorGray, client, colorReset),
		)
	}
}
