package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
)

func main() {
	// 命令行参数
	var (
		configFile = flag.String("config", "config.json", "Configuration file path")
		version    = flag.Bool("version", false, "Show version information")
	)
	flag.Parse()

	if *version {
		fmt.Println("Registry Proxy Server v1.0.0")
		fmt.Println("Built with go-containerregistry")
		return
	}

	// 加载配置
	config, err := LoadConfig(*configFile)
	if err != nil {
		fmt.Printf("Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// 初始化日志
	logger, err := initLogger(config.LogLevel)
	if err != nil {
		fmt.Printf("Failed to init logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	logger.Info("Starting Registry Proxy Server",
		zap.String("config", *configFile),
		zap.Int("port", config.Port))

	// 创建服务
	proxy, err := NewRegistryProxy(config)
	if err != nil {
		logger.Fatal("Failed to create registry proxy", zap.Error(err))
	}

	// 添加上游配置
	if err := setupUpstreams(proxy, config); err != nil {
		logger.Fatal("Failed to setup upstreams", zap.Error(err))
	}

	// 创建管理用户（如果不存在）
	if err := setupAdminUser(proxy, config); err != nil {
		logger.Warn("Failed to setup admin user", zap.Error(err))
	}

	// 启动后台服务
	go startBackgroundServices(proxy, logger)

	// 启动HTTP服务器
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- proxy.StartServer()
	}()

	// 等待信号或错误
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErrCh:
		logger.Error("Server error", zap.Error(err))
	case sig := <-sigCh:
		logger.Info("Received signal", zap.String("signal", sig.String()))
	}

	// 优雅停机
	gracefulShutdown(proxy, logger)
}

// LoadConfig 加载配置文件
func LoadConfig(configFile string) (*Config, error) {
	// 默认配置
	config := &Config{
		Port:               5000,
		TokenExpiry:        Duration(24 * time.Hour),
		MaxConcurrent:      50,
		BufferSize:         1024 * 1024, // 1MB
		EnableMetrics:      true,
		LogLevel:           "info",
		DataPersistence:    false,
		DataFile:           "/tmp/registry-proxy-data.json",
		StreamTimeout:      Duration(300 * time.Second), // 5分钟
		ConnectionPoolSize: 100,
		EnableCompression:  false, // 禁用压缩以提高流式性能
	}

	// 读取配置文件
	if _, err := os.Stat(configFile); err == nil {
		data, err := os.ReadFile(configFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}

		if err := json.Unmarshal(data, config); err != nil {
			return nil, fmt.Errorf("failed to parse config file: %w", err)
		}
	}

	// 从环境变量覆盖配置
	if port := os.Getenv("PROXY_PORT"); port != "" {
		fmt.Sscanf(port, "%d", &config.Port)
	}
	if logLevel := os.Getenv("LOG_LEVEL"); logLevel != "" {
		config.LogLevel = logLevel
	}
	if dataFile := os.Getenv("DATA_FILE"); dataFile != "" {
		config.DataFile = dataFile
		config.DataPersistence = true
	}

	return config, nil
}

// initLogger 初始化日志
func initLogger(level string) (*zap.Logger, error) {
	var config zap.Config

	if level == "debug" {
		config = zap.NewDevelopmentConfig()
	} else {
		config = zap.NewProductionConfig()
	}

	switch level {
	case "debug":
		config.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	case "info":
		config.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	case "warn":
		config.Level = zap.NewAtomicLevelAt(zap.WarnLevel)
	case "error":
		config.Level = zap.NewAtomicLevelAt(zap.ErrorLevel)
	default:
		config.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	}

	return config.Build()
}

// setupUpstreams 设置上游注册表
func setupUpstreams(proxy *RegistryProxy, config *Config) error {
	// 这里可以从配置文件或环境变量读取上游配置
	// 示例：添加DockerHub作为默认上游
	dockerhubConfig := &UpstreamConfig{
		Name:     "dockerhub",
		Registry: "index.docker.io",
		Username: os.Getenv("DOCKERHUB_USERNAME"),
		Password: os.Getenv("DOCKERHUB_PASSWORD"),
	}

	if err := proxy.AddUpstream(dockerhubConfig); err != nil {
		return fmt.Errorf("failed to add dockerhub upstream: %w", err)
	}

	// 添加其他上游（如果配置了的话）
	if gcrServiceAccount := os.Getenv("GCR_SERVICE_ACCOUNT"); gcrServiceAccount != "" {
		gcrConfig := &UpstreamConfig{
			Name:     "gcr",
			Registry: "gcr.io",
			// GCR需要特殊的认证处理
		}
		proxy.AddUpstream(gcrConfig)
	}

	return nil
}

// setupAdminUser 创建管理员用户
func setupAdminUser(proxy *RegistryProxy, config *Config) error {
	adminUsername := os.Getenv("ADMIN_USERNAME")
	adminPassword := os.Getenv("ADMIN_PASSWORD")

	if adminUsername == "" || adminPassword == "" {
		adminUsername = "admin"
		adminPassword = "admin123" // 生产环境应该使用强密码
	}

	userStore := proxy.store
	
	// 检查管理员用户是否已存在
	if _, err := userStore.GetUserByUsername(adminUsername); err == nil {
		return nil // 用户已存在
	}

	// 创建管理员用户
	adminUser := &User{
		Username:    adminUsername,
		Email:       "admin@registry-proxy.local",
		PasswordHash: adminPassword, // 将在CreateUser中被哈希
		Roles:       []string{"admin"},
		Permissions: map[string][]string{
			"*": {"*"}, // 全部权限
		},
		Enabled: true,
	}

	return userStore.CreateUser(adminUser)
}

// startBackgroundServices 启动后台服务
func startBackgroundServices(proxy *RegistryProxy, logger *zap.Logger) {
	// 启动数据持久化（如果启用）
	if proxy.config.DataPersistence {
		go startDataPersistence(proxy, logger)
	}
	
	// 启动会话清理任务，防止内存泄漏
	go func() {
		ticker := time.NewTicker(10 * time.Minute) // 每10分钟清理一次
		defer ticker.Stop()
		
		for {
			select {
			case <-ticker.C:
				proxy.proxyService.CleanupExpiredSessions()
				logger.Debug("Cleaned up expired upload sessions")
			}
		}
	}()
	
	// 启动认证token清理任务
	go func() {
		ticker := time.NewTicker(1 * time.Hour) // 每小时清理一次过期token
		defer ticker.Stop()
		
		for {
			select {
			case <-ticker.C:
				proxy.authService.CleanupExpiredTokens()
				logger.Debug("Cleaned up expired auth tokens")
			}
		}
	}()
}

// gracefulShutdown 优雅停机
func gracefulShutdown(proxy *RegistryProxy, logger *zap.Logger) {
	logger.Info("Starting graceful shutdown...")
	
	// 创建超时上下文
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	
	// 停止HTTP服务器
	if proxy.server != nil {
		if err := proxy.server.Shutdown(ctx); err != nil {
			logger.Error("Failed to shutdown server gracefully", zap.Error(err))
		} else {
			logger.Info("HTTP server stopped")
		}
	}
	
	// 等待活跃上传完成
	go waitForActiveUploads(proxy, logger, ctx)
	
	// 清理所有会话资源
	proxy.proxyService.CleanupExpiredSessions()
	
	// 停止内存存储
	if proxy.store != nil {
		proxy.store.Stop()
		logger.Info("Memory store stopped")
	}
	
	logger.Info("Graceful shutdown completed")
}

// waitForActiveUploads 等待活跃上传完成
func waitForActiveUploads(proxy *RegistryProxy, logger *zap.Logger, ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			logger.Warn("Timeout waiting for uploads to complete")
			return
		case <-ticker.C:
			// 统计活跃会话数
			activeCount := 0
			for _, shard := range proxy.proxyService.uploadSessions {
				shard.mutex.RLock()
				activeCount += len(shard.sessions)
				shard.mutex.RUnlock()
			}
			
			if activeCount == 0 {
				logger.Info("All upload sessions completed")
				return
			}
			
			logger.Info("Waiting for upload sessions to complete", 
				zap.Int("active_sessions", activeCount))
		}
	}
}

// 示例配置文件生成
func generateExampleConfig() *Config {
	return &Config{
		Port:            5000,
		TLSCert:         "", // 留空表示使用HTTP
		TLSKey:          "",
		TokenExpiry:     Duration(24 * time.Hour),
		MaxConcurrent:   10,
		BufferSize:      64 * 1024,
		EnableMetrics:   true,
		LogLevel:        "info",
		DataPersistence: false,
		DataFile:        "/tmp/registry-proxy-data.json",
		StreamTimeout:   Duration(300 * time.Second),
	}
}

// 健康检查端点扩展
func (rp *RegistryProxy) HandleHealthDetailed() map[string]interface{} {
	health := make(map[string]interface{})
	
	// 基础健康状态
	health["status"] = "healthy"
	health["timestamp"] = time.Now().UTC()
	health["version"] = "1.0.0"
	
	// 内存存储状态
	if rp.store != nil {
		health["storage"] = "healthy"
		health["storage_stats"] = rp.store.GetStats()
	} else {
		health["storage"] = "unhealthy"
		health["status"] = "degraded"
	}
	
	// 上游状态
	upstreamHealth := make(map[string]string)
	for name, upstream := range rp.upstreams {
		// 简单的连通性检查
		if upstream.Registry != "" {
			upstreamHealth[name] = "healthy" // 简化实现
		} else {
			upstreamHealth[name] = "unknown"
		}
	}
	health["upstreams"] = upstreamHealth
	
	// 活跃会话数
	activeCount := 0
	for _, shard := range rp.proxyService.uploadSessions {
		shard.mutex.RLock()
		activeCount += len(shard.sessions)
		shard.mutex.RUnlock()
	}
	health["active_uploads"] = activeCount
	
	return health
}

// 示例客户端配置生成
func generateClientDockerConfig(proxyHost string, username, password string) map[string]interface{} {
	config := map[string]interface{}{
		"auths": map[string]interface{}{
			proxyHost: map[string]interface{}{
				"username": username,
				"password": password,
			},
		},
		"experimental": "enabled",
	}
	
	return config
}

// startDataPersistence 启动数据持久化
func startDataPersistence(proxy *RegistryProxy, logger *zap.Logger) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			saveData(proxy, logger)
		}
	}
}

// saveData 保存数据到文件
func saveData(proxy *RegistryProxy, logger *zap.Logger) {
	data, err := proxy.store.ExportData()
	if err != nil {
		logger.Error("Failed to export data", zap.Error(err))
		return
	}

	if err := os.WriteFile(proxy.config.DataFile, data, 0600); err != nil {
		logger.Error("Failed to save data to file", zap.Error(err))
		return
	}

	logger.Debug("Data saved successfully", zap.String("file", proxy.config.DataFile))
} 