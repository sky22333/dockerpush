package main

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"encoding/json"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.uber.org/zap"
)

// Duration 自定义时间类型，支持JSON字符串和数字解析
type Duration time.Duration

// UnmarshalJSON 自定义JSON解析方法
func (d *Duration) UnmarshalJSON(data []byte) error {
	var v interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	
	switch value := v.(type) {
	case string:
		// 解析时间字符串，如 "24h", "300s", "5m"
		duration, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		*d = Duration(duration)
	case float64:
		// 解析数字（秒数），如 86400, 300
		*d = Duration(time.Duration(value) * time.Second)
	default:
		return fmt.Errorf("invalid duration type: %T", value)
	}
	return nil
}

// ToDuration 转换为标准time.Duration
func (d Duration) ToDuration() time.Duration {
	return time.Duration(d)
}

// RegistryProxy 主代理服务
type RegistryProxy struct {
	server       *http.Server
	authService  *MemoryAuthService
	proxyService *ProxyService
	store        *MemoryStore
	upstreams    map[string]*UpstreamConfig
	logger       *zap.Logger
	config       *Config
}

// Config 服务配置
type Config struct {
	Port               int      `json:"port"`
	TLSCert            string   `json:"tls_cert"`
	TLSKey             string   `json:"tls_key"`
	TokenExpiry        Duration `json:"token_expiry"`
	MaxConcurrent      int      `json:"max_concurrent"`
	BufferSize         int      `json:"buffer_size"`
	LogLevel           string   `json:"log_level"`
	DataPersistence    bool     `json:"data_persistence"`
	DataFile           string   `json:"data_file"`
	ConnectionPoolSize int      `json:"connection_pool_size"`
	RequestTimeout     Duration `json:"request_timeout"`
}

// UpstreamConfig 上游注册表配置
type UpstreamConfig struct {
	Name      string                `json:"name"`
	Registry  string                `json:"registry"`
	Username  string                `json:"username"`
	Password  string                `json:"password"`
	Auth      authn.Authenticator   `json:"-"`
	Transport http.RoundTripper     `json:"-"`
	Pusher    *remote.Pusher        `json:"-"`
	RoutePattern string            `json:"route_pattern"`
}

// ProxyService 代理服务
type ProxyService struct {
	logger         *zap.Logger
	semaphore      chan struct{} // 控制并发
	bufferSize     int
	
	// 分片会话管理，减少锁争用
	uploadSessions []*sessionShard
	shardCount     int
}

// sessionShard 会话分片，减少全局锁争用
type sessionShard struct {
	sessions map[string]*UploadSessionManager
	mutex    sync.RWMutex
}

// 计算分片索引
func (ps *ProxyService) getShardIndex(uuid string) int {
	hash := 0
	for _, c := range uuid {
		hash = hash*31 + int(c)
	}
	return hash % ps.shardCount
}

// GetUploadSession 线程安全地获取上传会话
func (ps *ProxyService) GetUploadSession(uuid string) (*UploadSessionManager, bool) {
	shardIndex := ps.getShardIndex(uuid)
	shard := ps.uploadSessions[shardIndex]
	
	shard.mutex.RLock()
	session, exists := shard.sessions[uuid]
	shard.mutex.RUnlock()
	
	// 检查会话是否已过期或关闭
	if exists && (session.IsClosed() || time.Since(session.session.StartTime) > 24*time.Hour) {
		ps.DeleteUploadSession(uuid)
		return nil, false
	}
	
	return session, exists
}

// SetUploadSession 线程安全地设置上传会话
func (ps *ProxyService) SetUploadSession(uuid string, session *UploadSessionManager) {
	shardIndex := ps.getShardIndex(uuid)
	shard := ps.uploadSessions[shardIndex]
	
	shard.mutex.Lock()
	shard.sessions[uuid] = session
	shard.mutex.Unlock()
}

// DeleteUploadSession 线程安全地删除上传会话
func (ps *ProxyService) DeleteUploadSession(uuid string) {
	shardIndex := ps.getShardIndex(uuid)
	shard := ps.uploadSessions[shardIndex]
	
	shard.mutex.Lock()
	if session, exists := shard.sessions[uuid]; exists {
		session.Close() // 优雅关闭资源
		delete(shard.sessions, uuid)
	}
	shard.mutex.Unlock()
}

// CleanupExpiredSessions 清理过期会话，防止内存泄漏
func (ps *ProxyService) CleanupExpiredSessions() {
	expireTime := 24 * time.Hour
	cutoff := time.Now().Add(-expireTime)
	
	for _, shard := range ps.uploadSessions {
		shard.mutex.Lock()
		for uuid, session := range shard.sessions {
			if session.IsClosed() || session.session.StartTime.Before(cutoff) {
				session.Close()
				delete(shard.sessions, uuid)
			}
		}
		shard.mutex.Unlock()
	}
}

// UploadSessionManager 精简的上传会话管理器
type UploadSessionManager struct {
	session    *UploadSession
	upstream   *UpstreamConfig
	ctx        context.Context
	cancel     context.CancelFunc
	mutex      sync.RWMutex
	closed     int32 // 原子操作标记，防止重复清理
	cleanOnce  sync.Once // 确保清理操作只执行一次
}

// Close 优雅关闭会话管理器，防止资源泄漏
func (usm *UploadSessionManager) Close() error {
	// 使用原子操作检查是否已关闭
	if !atomic.CompareAndSwapInt32(&usm.closed, 0, 1) {
		return nil // 已经关闭
	}
	
	var closeErr error
	usm.cleanOnce.Do(func() {
		// 取消上下文
		if usm.cancel != nil {
			usm.cancel()
		}
	})
	
	return closeErr
}

// IsClosed 检查会话是否已关闭
func (usm *UploadSessionManager) IsClosed() bool {
	return atomic.LoadInt32(&usm.closed) == 1
}

// SafeUpdateOffset 线程安全地更新偏移量
func (usm *UploadSessionManager) SafeUpdateOffset(delta int64) {
	usm.mutex.Lock()
	defer usm.mutex.Unlock()
	usm.session.Offset += delta
}

// MemoryAuthService 基于内存的认证服务
type MemoryAuthService struct {
	store         *MemoryStore
	tokenExpiry   time.Duration
	logger        *zap.Logger
	jwtSigningKey []byte
	// 移除：clientAuthCache - 完全透传模式下不需要缓存
}

// TokenResponse Docker认证令牌响应
type TokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// NewRegistryProxy 创建新的注册表代理
func NewRegistryProxy(config *Config) (*RegistryProxy, error) {
	logger, _ := zap.NewProduction()
	
	// 初始化内存存储
	store := NewMemoryStore()
	
	// 生成JWT签名密钥
	jwtKey := make([]byte, 32)
	cryptorand.Read(jwtKey)
	
	// 初始化认证服务
	authService := &MemoryAuthService{
		store:         store,
		tokenExpiry:   config.TokenExpiry.ToDuration(),
		logger:        logger,
		jwtSigningKey: jwtKey,
	}
	
	// 初始化代理服务
	proxyService := &ProxyService{
		logger:         logger,
		semaphore:      make(chan struct{}, config.MaxConcurrent),
		bufferSize:     config.BufferSize,
		uploadSessions: make([]*sessionShard, config.ConnectionPoolSize),
		shardCount:     config.ConnectionPoolSize,
	}
	
	// 初始化分片
	for i := 0; i < config.ConnectionPoolSize; i++ {
		proxyService.uploadSessions[i] = &sessionShard{
			sessions: make(map[string]*UploadSessionManager),
		}
	}
	
	proxy := &RegistryProxy{
		authService:  authService,
		proxyService: proxyService,
		store:        store,
		upstreams:    make(map[string]*UpstreamConfig),
		logger:       logger,
		config:       config,
	}
	
	// 如果启用数据持久化，尝试加载数据
	if config.DataPersistence && config.DataFile != "" {
		proxy.loadPersistedData()
	}
	
	return proxy, nil
}

// loadPersistedData 加载持久化数据
func (rp *RegistryProxy) loadPersistedData() {
	if _, err := os.Stat(rp.config.DataFile); os.IsNotExist(err) {
		rp.logger.Info("No persistent data file found, starting fresh")
		return
	}

	data, err := os.ReadFile(rp.config.DataFile)
	if err != nil {
		rp.logger.Error("Failed to read persistent data", zap.Error(err))
		return
	}

	if err := rp.store.ImportData(data); err != nil {
		rp.logger.Error("Failed to import persistent data", zap.Error(err))
		return
	}

	rp.logger.Info("Persistent data loaded successfully")
}

// AddUpstream 添加上游注册表
func (rp *RegistryProxy) AddUpstream(config *UpstreamConfig) error {
	// 初始化认证器
	if config.Username != "" && config.Password != "" {
		config.Auth = &authn.Basic{
			Username: config.Username,
			Password: config.Password,
		}
	} else {
		config.Auth = authn.Anonymous
	}
	
	// 创建优化的传输层
	config.Transport = &http.Transport{
		MaxIdleConns:           200,                // 增加最大空闲连接数
		MaxIdleConnsPerHost:    50,                 // 增加每个主机的最大空闲连接数
		IdleConnTimeout:        90 * time.Second,   // 空闲连接超时
		TLSHandshakeTimeout:    10 * time.Second,   // TLS握手超时
		ResponseHeaderTimeout:  30 * time.Second,   // 响应头超时
		ExpectContinueTimeout:  1 * time.Second,    // Expect: 100-continue超时
		MaxConnsPerHost:        100,                // 每个主机的最大连接数
		DisableCompression:     true,               // 禁用压缩以提高流式传输性能
		WriteBufferSize:        rp.config.BufferSize, // 写缓冲区大小
		ReadBufferSize:         rp.config.BufferSize,  // 读缓冲区大小
	}
	
	// 创建推送器
	pusher, err := remote.NewPusher(
		remote.WithAuth(config.Auth),
		remote.WithTransport(config.Transport),
		remote.WithJobs(rp.config.MaxConcurrent/2), // 使用一半的并发数
	)
	if err != nil {
		return fmt.Errorf("failed to create pusher: %w", err)
	}
	config.Pusher = pusher
	
	rp.upstreams[config.Name] = config
	
	rp.logger.Info("Added upstream registry", 
		zap.String("name", config.Name),
		zap.String("registry", config.Registry),
		zap.String("route_pattern", config.RoutePattern))
	
	return nil
}

// StartServer 启动服务器
func (rp *RegistryProxy) StartServer() error {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	
	// 中间件
	router.Use(rp.LoggingMiddleware())
	router.Use(rp.CORSMiddleware())
	router.Use(rp.ErrorMiddleware())
	
	// Docker Registry API V2 路由
	v2 := router.Group("/v2")
	{
		v2.GET("/", rp.HandlePing)
		v2.GET("/auth", rp.HandleAuth)
		
		// 认证中间件
		authGroup := v2.Group("/")
		authGroup.Use(rp.AuthMiddleware())
		{
			// Manifest API
			authGroup.GET("/:name/manifests/:reference", rp.HandleManifestGet)
			authGroup.PUT("/:name/manifests/:reference", rp.HandleManifestPut)
			authGroup.DELETE("/:name/manifests/:reference", rp.HandleManifestDelete)
			authGroup.HEAD("/:name/manifests/:reference", rp.HandleManifestHead)
			
			// Blob API
			authGroup.GET("/:name/blobs/:digest", rp.HandleBlobGet)
			authGroup.HEAD("/:name/blobs/:digest", rp.HandleBlobHead)
			authGroup.DELETE("/:name/blobs/:digest", rp.HandleBlobDelete)
			
			// Upload API
			authGroup.POST("/:name/blobs/uploads/", rp.HandleBlobUploadStart)
			authGroup.PATCH("/:name/blobs/uploads/:uuid", rp.HandleBlobUploadChunk)
			authGroup.PUT("/:name/blobs/uploads/:uuid", rp.HandleBlobUploadComplete)
			authGroup.DELETE("/:name/blobs/uploads/:uuid", rp.HandleBlobUploadCancel)
			authGroup.GET("/:name/blobs/uploads/:uuid", rp.HandleBlobUploadStatus)
			
			// Catalog API
			authGroup.GET("/_catalog", rp.HandleCatalog)
			authGroup.GET("/:name/tags/list", rp.HandleTagsList)
		}
	}
	
	// 健康检查
	router.GET("/health", rp.HandleHealth)
	router.GET("/metrics", rp.HandleMetrics)
	
	rp.server = &http.Server{
		Addr:         fmt.Sprintf(":%d", rp.config.Port),
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	
	rp.logger.Info("Starting registry proxy server", 
		zap.Int("port", rp.config.Port))
	
	if rp.config.TLSCert != "" && rp.config.TLSKey != "" {
		return rp.server.ListenAndServeTLS(rp.config.TLSCert, rp.config.TLSKey)
	}
	return rp.server.ListenAndServe()
}

// HandleAuth 处理Docker认证 - 支持透传模式
func (rp *RegistryProxy) HandleAuth(c *gin.Context) {
	service := c.Query("service")
	scope := c.Query("scope")
	
	// 验证基本认证格式
	username, _, ok := c.Request.BasicAuth()
	if !ok || username == "" {
		c.Header("WWW-Authenticate", `Basic realm="Registry Realm"`)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	
	rp.logger.Info("Client authentication request", 
		zap.String("username", username))
	
	// 生成访问令牌
	token, err := rp.authService.GenerateToken(username, service, scope)
	if err != nil {
		rp.logger.Error("Failed to generate token", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token generation failed"})
		return
	}
	
	// 🔥 移除：不再存储客户端认证信息，实现真正透传
	// rp.storeClientAuth(token, username, password)
	
	response := TokenResponse{
		Token:       token,
		AccessToken: token,
		ExpiresIn:   int(rp.config.TokenExpiry.ToDuration().Seconds()),
	}
	
	c.JSON(http.StatusOK, response)
}

// HandleManifestPut 流式处理Manifest推送
func (rp *RegistryProxy) HandleManifestPut(c *gin.Context) {
	repository := c.Param("name")
	reference := c.Param("reference")
	
	upstream := rp.determineUpstream(repository)
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	// 构建上游URL
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", upstream.Registry, repository, reference)
	
	// 创建上游PUT请求，直接传递请求体
	req, err := http.NewRequestWithContext(c.Request.Context(), "PUT", upstreamURL, c.Request.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upstream request"})
		return
	}
	
	// 复制必要的头部
	rp.copyHeaders(c.Request, req, []string{"Content-Type", "Content-Length", "Authorization"})
	
	// 设置默认Content-Type
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
	}
	
	// 添加认证 - 使用透传模式
	rp.setUpstreamAuthWithClient(req, upstream, c)
	
	// 发起上游请求
	client := &http.Client{
		Transport: upstream.Transport,
		Timeout:   rp.config.RequestTimeout.ToDuration(),
	}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	defer resp.Body.Close()
	
	// 复制响应头部和状态码
	rp.copyResponseHeaders(resp, c)
	
	// 如果成功，设置Location头部
	if resp.StatusCode == http.StatusCreated {
	c.Header("Location", fmt.Sprintf("/v2/%s/manifests/%s", repository, reference))
	}
	
	c.Status(resp.StatusCode)
	
	rp.logger.Info("Manifest pushed successfully", 
		zap.String("repository", repository), 
		zap.String("reference", reference),
		zap.String("upstream", upstream.Registry),
		zap.Int("status", resp.StatusCode))
}

// HandleBlobUploadStart 开始流式blob上传
func (rp *RegistryProxy) HandleBlobUploadStart(c *gin.Context) {
	repository := c.Param("name")
	
	upstream := rp.determineUpstream(repository)
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	// 向上游发起上传开始请求
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/blobs/uploads/", upstream.Registry, repository)
	
	req, err := http.NewRequestWithContext(c.Request.Context(), "POST", upstreamURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upstream request"})
		return
	}
	
	// 复制必要的头部
	rp.copyHeaders(c.Request, req, []string{"Authorization"})
	
	// 添加认证 - 使用透传模式
	rp.setUpstreamAuthWithClient(req, upstream, c)
	
	// 发起上游请求
	client := &http.Client{
		Transport: upstream.Transport,
		Timeout:   rp.config.RequestTimeout.ToDuration(),
	}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	defer resp.Body.Close()
	
	// 从上游响应中提取upload UUID - 增强版
	location := resp.Header.Get("Location")
	if location == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream did not provide upload location"})
		return
	}
	
	// 🔥 健壮的UUID提取逻辑
	uploadUUID := rp.extractUUIDFromLocation(location)
	if uploadUUID == "" {
		uploadUUID = rp.generateUploadUUID()
		rp.logger.Warn("Failed to extract UUID from location, generated new one",
			zap.String("location", location),
			zap.String("generated_uuid", uploadUUID))
	}
	
	// 创建流式上传会话管理器
	ctx, cancel := context.WithCancel(c.Request.Context())
	
	sessionManager := &UploadSessionManager{
		session: &UploadSession{
		UUID:       uploadUUID,
		Repository: repository,
		StartTime:  time.Now(),
			Offset:     0,
		},
		upstream:   upstream,
		ctx:        ctx,
		cancel:     cancel,
		mutex:      sync.RWMutex{},
		closed:     0,
	}
	
	// 存储会话管理器
	rp.proxyService.SetUploadSession(uploadUUID, sessionManager)
	
	// 存储会话信息
	rp.store.SetUploadSession(uploadUUID, sessionManager.session)
	
	// 调整Location头部指向我们的代理
	proxyLocation := fmt.Sprintf("/v2/%s/blobs/uploads/%s", repository, uploadUUID)
	
	// 复制响应头部
	rp.copyResponseHeaders(resp, c)
	c.Header("Location", proxyLocation)
	c.Status(resp.StatusCode)
	
	rp.logger.Info("Upload session started", 
		zap.String("repository", repository),
		zap.String("uploadUUID", uploadUUID),
		zap.String("upstream", upstream.Registry))
}

// HandleBlobUploadChunk 处理流式分块上传
func (rp *RegistryProxy) HandleBlobUploadChunk(c *gin.Context) {
	repository := c.Param("name")
	uploadUUID := c.Param("uuid")
	
	upstream := rp.determineUpstream(repository)
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	// 获取上传会话管理器
	sessionManager, exists := rp.proxyService.GetUploadSession(uploadUUID)
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}
	
	// 构建上游PATCH请求URL
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/blobs/uploads/%s", upstream.Registry, repository, uploadUUID)
	
	// 获取Content-Range头部
	contentRange := c.GetHeader("Content-Range")
	contentLength := c.GetHeader("Content-Length")
	
	// 创建上游PATCH请求，直接流式传递请求体
	req, err := http.NewRequestWithContext(sessionManager.ctx, "PATCH", upstreamURL, c.Request.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upstream request"})
		return
	}
	
	// 复制重要头部
	rp.copyHeaders(c.Request, req, []string{"Content-Length", "Content-Type", "Content-Range", "Authorization"})
	
	// 添加认证 - 使用透传模式
	rp.setUpstreamAuthWithClient(req, upstream, c)
	
	// 发起上游请求
	client := &http.Client{
		Transport: upstream.Transport,
		Timeout:   rp.config.RequestTimeout.ToDuration(),
	}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		rp.logger.Error("Upstream request failed in chunk upload", 
			zap.Error(err),
			zap.String("repository", repository),
			zap.String("uploadUUID", uploadUUID))
		return
	}
	defer resp.Body.Close()
	
	// 更新会话偏移量
	if contentLength != "" {
		var length int64
		if n, parseErr := fmt.Sscanf(contentLength, "%d", &length); parseErr == nil && n == 1 && length > 0 {
			sessionManager.SafeUpdateOffset(length) // 使用线程安全的更新方法
		}
	}
	
	// 从上游响应中获取实际的Range
	upstreamRange := resp.Header.Get("Range")
	if upstreamRange != "" {
		c.Header("Range", upstreamRange)
	} else {
		c.Header("Range", fmt.Sprintf("0-%d", sessionManager.session.Offset-1))
	}
	
	// 更新存储中的会话
	rp.store.SetUploadSession(uploadUUID, sessionManager.session)
	
	// 复制其他响应头部
	rp.copyResponseHeaders(resp, c)
	
	// 设置Location头部指向我们的代理
	location := fmt.Sprintf("/v2/%s/blobs/uploads/%s", repository, uploadUUID)
	c.Header("Location", location)
	c.Status(resp.StatusCode)
	
	rp.logger.Debug("Chunk uploaded successfully", 
		zap.String("repository", repository),
		zap.String("uploadUUID", uploadUUID),
		zap.String("contentRange", contentRange),
		zap.String("upstream", upstream.Registry),
		zap.Int("status", resp.StatusCode))
}

// 中间件实现
func (rp *RegistryProxy) AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.Header("WWW-Authenticate", `Bearer realm="/v2/auth",service="registry"`)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		
		// 验证Bearer Token
		if !strings.HasPrefix(authHeader, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid auth format"})
			return
		}
		
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if !rp.authService.ValidateToken(token) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
		
		// 🔥 将token存储到上下文中，供后续透传使用
		c.Set("auth_token", token)
		
		c.Next()
	}
}

// determineUpstream 智能上游路由选择
func (rp *RegistryProxy) determineUpstream(repository string) *UpstreamConfig {
	// 🔥 智能路由逻辑：基于repository名称匹配上游
	
	// 1. 精确匹配：查找专门为该repository配置的上游
	for _, upstream := range rp.upstreams {
		if upstream.RoutePattern != "" {
			// 支持通配符匹配，如 "library/*", "gcr.io/*"
			if matched, _ := filepath.Match(upstream.RoutePattern, repository); matched {
				rp.logger.Debug("Matched upstream by pattern", 
					zap.String("repository", repository),
					zap.String("pattern", upstream.RoutePattern),
					zap.String("upstream", upstream.Name))
				return upstream
			}
		}
	}
	
	// 2. 基于registry前缀匹配（如gcr.io/project/image -> gcr.io上游）
	if strings.Contains(repository, "/") {
		parts := strings.Split(repository, "/")
		registryPrefix := parts[0]
		
		for _, upstream := range rp.upstreams {
			if strings.Contains(upstream.Registry, registryPrefix) {
				rp.logger.Debug("Matched upstream by registry prefix",
					zap.String("repository", repository),
					zap.String("prefix", registryPrefix),
					zap.String("upstream", upstream.Name))
				return upstream
			}
		}
	}
	
	// 3. 默认回退：使用第一个配置的上游
	for _, upstream := range rp.upstreams {
		rp.logger.Debug("Using default upstream",
			zap.String("repository", repository),
			zap.String("upstream", upstream.Name))
		return upstream
	}
	
	return nil
}

func (rp *RegistryProxy) generateUploadUUID() string {
	// 生成加密安全的UUID v4
	b := make([]byte, 16)
	cryptorand.Read(b)
	
	// 设置版本号和变体位
	b[6] = (b[6] & 0x0f) | 0x40 // 版本4
	b[8] = (b[8] & 0x3f) | 0x80 // 变体位
	
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", 
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// UploadSession 上传会话
type UploadSession struct {
	UUID       string    `json:"uuid"`
	Repository string    `json:"repository"`
	StartTime  time.Time `json:"start_time"`
	Offset     int64     `json:"offset"`
}

// 其他处理函数的声明（简化）
func (rp *RegistryProxy) HandlePing(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{})
}

func (rp *RegistryProxy) HandleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "healthy"})
}

func (rp *RegistryProxy) HandleMetrics(c *gin.Context) {
	// Prometheus metrics implementation
	c.String(http.StatusOK, "# metrics here")
}

// 中间件实现
func (rp *RegistryProxy) LoggingMiddleware() gin.HandlerFunc {
	return gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		return fmt.Sprintf("%s - [%s] \"%s %s %s %d %s \"%s\" %s\"\n",
			param.ClientIP,
			param.TimeStamp.Format(time.RFC1123),
			param.Method,
			param.Path,
			param.Request.Proto,
			param.StatusCode,
			param.Latency,
			param.Request.UserAgent(),
			param.ErrorMessage,
		)
	})
}

func (rp *RegistryProxy) CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, HEAD, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
		
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		
		c.Next()
	}
}

func (rp *RegistryProxy) ErrorMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		
		if len(c.Errors) > 0 {
			err := c.Errors.Last()
			rp.logger.Error("Request error", 
				zap.String("path", c.Request.URL.Path),
				zap.Error(err))
		}
	}
}

// HandleManifestGet 流式获取Manifest
func (rp *RegistryProxy) HandleManifestGet(c *gin.Context) {
	repository := c.Param("name")
	reference := c.Param("reference")
	
	upstream := rp.determineUpstream(repository)
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	// 构建上游URL
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", upstream.Registry, repository, reference)
	
	// 创建上游请求
	req, err := http.NewRequestWithContext(c.Request.Context(), "GET", upstreamURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upstream request"})
		return
	}
	
	// 复制必要的头部
	rp.copyHeaders(c.Request, req, []string{"Accept", "Authorization"})
	
	// 添加认证 - 使用透传模式
	rp.setUpstreamAuthWithClient(req, upstream, c)
	
	// 发起上游请求
	client := &http.Client{
		Transport: upstream.Transport,
		Timeout:   rp.config.RequestTimeout.ToDuration(),
	}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	defer resp.Body.Close()
	
	// 流式转发响应
	rp.streamResponse(c, resp)
}

// HandleManifestHead 获取Manifest头部信息
func (rp *RegistryProxy) HandleManifestHead(c *gin.Context) {
	repository := c.Param("name")
	reference := c.Param("reference")
	
	upstream := rp.determineUpstream(repository)
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", upstream.Registry, repository, reference)
	
	req, err := http.NewRequestWithContext(c.Request.Context(), "HEAD", upstreamURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upstream request"})
		return
	}
	
	rp.copyHeaders(c.Request, req, []string{"Accept", "Authorization"})
	
	rp.setUpstreamAuthWithClient(req, upstream, c)
	
	client := &http.Client{
		Transport: upstream.Transport,
		Timeout:   rp.config.RequestTimeout.ToDuration(),
	}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	defer resp.Body.Close()
	
	// 复制响应头部
	rp.copyResponseHeaders(resp, c)
	c.Status(resp.StatusCode)
}

// HandleManifestDelete 删除Manifest
func (rp *RegistryProxy) HandleManifestDelete(c *gin.Context) {
	repository := c.Param("name")
	reference := c.Param("reference")
	
	upstream := rp.determineUpstream(repository)
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", upstream.Registry, repository, reference)
	
	req, err := http.NewRequestWithContext(c.Request.Context(), "DELETE", upstreamURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upstream request"})
		return
	}
	
	rp.copyHeaders(c.Request, req, []string{"Authorization"})
	
	rp.setUpstreamAuthWithClient(req, upstream, c)
	
	client := &http.Client{
		Transport: upstream.Transport,
		Timeout:   rp.config.RequestTimeout.ToDuration(),
	}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	defer resp.Body.Close()
	
	rp.copyResponseHeaders(resp, c)
	c.Status(resp.StatusCode)
}

// HandleBlobGet 流式获取Blob
func (rp *RegistryProxy) HandleBlobGet(c *gin.Context) {
	repository := c.Param("name")
	digest := c.Param("digest")
	
	upstream := rp.determineUpstream(repository)
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/blobs/%s", upstream.Registry, repository, digest)
	
	req, err := http.NewRequestWithContext(c.Request.Context(), "GET", upstreamURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upstream request"})
		return
	}
	
	rp.copyHeaders(c.Request, req, []string{"Range", "Authorization"})
	
	rp.setUpstreamAuthWithClient(req, upstream, c)
	
	client := &http.Client{
		Transport: upstream.Transport,
		Timeout:   rp.config.RequestTimeout.ToDuration(),
	}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	defer resp.Body.Close()
	
	// 流式转发Blob数据
	rp.streamResponse(c, resp)
}

// HandleBlobHead 获取Blob头部信息
func (rp *RegistryProxy) HandleBlobHead(c *gin.Context) {
	repository := c.Param("name")
	digest := c.Param("digest")
	
	upstream := rp.determineUpstream(repository)
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/blobs/%s", upstream.Registry, repository, digest)
	
	req, err := http.NewRequestWithContext(c.Request.Context(), "HEAD", upstreamURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upstream request"})
		return
	}
	
	rp.copyHeaders(c.Request, req, []string{"Authorization"})
	
	rp.setUpstreamAuthWithClient(req, upstream, c)
	
	client := &http.Client{
		Transport: upstream.Transport,
		Timeout:   rp.config.RequestTimeout.ToDuration(),
	}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	defer resp.Body.Close()
	
	rp.copyResponseHeaders(resp, c)
	c.Status(resp.StatusCode)
}

// HandleBlobDelete 删除Blob
func (rp *RegistryProxy) HandleBlobDelete(c *gin.Context) {
	repository := c.Param("name")
	digest := c.Param("digest")
	
	upstream := rp.determineUpstream(repository)
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/blobs/%s", upstream.Registry, repository, digest)
	
	req, err := http.NewRequestWithContext(c.Request.Context(), "DELETE", upstreamURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upstream request"})
		return
	}
	
	rp.copyHeaders(c.Request, req, []string{"Authorization"})
	
	rp.setUpstreamAuthWithClient(req, upstream, c)
	
	client := &http.Client{
		Transport: upstream.Transport,
		Timeout:   rp.config.RequestTimeout.ToDuration(),
	}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	defer resp.Body.Close()
	
	rp.copyResponseHeaders(resp, c)
	c.Status(resp.StatusCode)
}

// HandleBlobUploadComplete 完成流式Blob上传
func (rp *RegistryProxy) HandleBlobUploadComplete(c *gin.Context) {
	repository := c.Param("name")
	uploadUUID := c.Param("uuid")
	digest := c.Query("digest")
	
	upstream := rp.determineUpstream(repository)
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	// 获取上传会话管理器
	sessionManager, exists := rp.proxyService.GetUploadSession(uploadUUID)
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}
	
	// 构建完成上传的URL
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/blobs/uploads/%s", upstream.Registry, repository, uploadUUID)
	if digest != "" {
		upstreamURL += "?digest=" + digest
	}
	
	// 创建上游PUT请求
	req, err := http.NewRequestWithContext(c.Request.Context(), "PUT", upstreamURL, c.Request.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upstream request"})
		return
	}
	
	// 复制头部
	rp.copyHeaders(c.Request, req, []string{"Content-Length", "Content-Type", "Authorization"})
	
	rp.setUpstreamAuthWithClient(req, upstream, c)
	
	// 发起上游请求
	client := &http.Client{
		Transport: upstream.Transport,
		Timeout:   rp.config.RequestTimeout.ToDuration(),
	}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	defer resp.Body.Close()
	
	// 清理上传会话
	sessionManager.Close() // 使用新的优雅关闭方法
	rp.proxyService.DeleteUploadSession(uploadUUID)
	
	// 删除存储中的会话
	rp.store.DeleteUploadSession(uploadUUID)
	
	// 转发响应
	rp.copyResponseHeaders(resp, c)
	c.Status(resp.StatusCode)
}

// HandleBlobUploadCancel 取消上传
func (rp *RegistryProxy) HandleBlobUploadCancel(c *gin.Context) {
	repository := c.Param("name")
	uploadUUID := c.Param("uuid")
	
	upstream := rp.determineUpstream(repository)
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	// 清理本地会话
	sessionManager, exists := rp.proxyService.GetUploadSession(uploadUUID)
	if exists {
		sessionManager.Close() // 使用新的优雅关闭方法
		rp.proxyService.DeleteUploadSession(uploadUUID)
	}
	
	rp.store.DeleteUploadSession(uploadUUID)
	
	// 向上游发送取消请求
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/blobs/uploads/%s", upstream.Registry, repository, uploadUUID)
	
	req, err := http.NewRequestWithContext(c.Request.Context(), "DELETE", upstreamURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upstream request"})
		return
	}
	
	rp.setUpstreamAuthWithClient(req, upstream, c)
	
	client := &http.Client{
		Transport: upstream.Transport,
		Timeout:   rp.config.RequestTimeout.ToDuration(),
	}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	defer resp.Body.Close()
	
	c.Status(resp.StatusCode)
}

// HandleBlobUploadStatus 获取上传状态
func (rp *RegistryProxy) HandleBlobUploadStatus(c *gin.Context) {
	repository := c.Param("name")
	uploadUUID := c.Param("uuid")
	
	upstream := rp.determineUpstream(repository)
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/blobs/uploads/%s", upstream.Registry, repository, uploadUUID)
	
	req, err := http.NewRequestWithContext(c.Request.Context(), "GET", upstreamURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upstream request"})
		return
	}
	
	rp.setUpstreamAuthWithClient(req, upstream, c)
	
	client := &http.Client{
		Transport: upstream.Transport,
		Timeout:   rp.config.RequestTimeout.ToDuration(),
	}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	defer resp.Body.Close()
	
	rp.copyResponseHeaders(resp, c)
	c.Status(resp.StatusCode)
}

// HandleCatalog 获取仓库目录
func (rp *RegistryProxy) HandleCatalog(c *gin.Context) {
	// 选择一个上游进行代理
	var upstream *UpstreamConfig
	for _, up := range rp.upstreams {
		upstream = up
		break
	}
	
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	upstreamURL := fmt.Sprintf("https://%s/v2/_catalog", upstream.Registry)
	
	req, err := http.NewRequestWithContext(c.Request.Context(), "GET", upstreamURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upstream request"})
		return
	}
	
	// 复制查询参数
	req.URL.RawQuery = c.Request.URL.RawQuery
	
	rp.setUpstreamAuthWithClient(req, upstream, c)
	
	client := &http.Client{
		Transport: upstream.Transport,
		Timeout:   rp.config.RequestTimeout.ToDuration(),
	}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	defer resp.Body.Close()
	
	rp.streamResponse(c, resp)
}

// HandleTagsList 获取标签列表
func (rp *RegistryProxy) HandleTagsList(c *gin.Context) {
	repository := c.Param("name")
	
	upstream := rp.determineUpstream(repository)
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/tags/list", upstream.Registry, repository)
	
	req, err := http.NewRequestWithContext(c.Request.Context(), "GET", upstreamURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upstream request"})
		return
	}
	
	// 复制查询参数
	req.URL.RawQuery = c.Request.URL.RawQuery
	
	rp.setUpstreamAuthWithClient(req, upstream, c)
	
	client := &http.Client{
		Transport: upstream.Transport,
		Timeout:   rp.config.RequestTimeout.ToDuration(),
	}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	defer resp.Body.Close()
	
	rp.streamResponse(c, resp)
}

// streamResponse 流式转发HTTP响应
func (rp *RegistryProxy) streamResponse(c *gin.Context, resp *http.Response) {
	// 复制状态码
	c.Status(resp.StatusCode)
	
	// 复制响应头部
	rp.copyResponseHeaders(resp, c)
	
	// 流式复制响应体
	writer := c.Writer
	reader := resp.Body
	
	// 使用缓冲区进行流式传输
	buffer := make([]byte, rp.config.BufferSize)
	
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			if _, writeErr := writer.Write(buffer[:n]); writeErr != nil {
				rp.logger.Error("Failed to write response", zap.Error(writeErr))
				return
			}
			// 立即刷新缓冲区，确保流式传输
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				rp.logger.Error("Error reading upstream response", zap.Error(err))
			}
			break
		}
	}
}

// copyHeaders 复制请求头部
func (rp *RegistryProxy) copyHeaders(src *http.Request, dst *http.Request, headers []string) {
	for _, header := range headers {
		if value := src.Header.Get(header); value != "" {
			dst.Header.Set(header, value)
		}
	}
}

// copyResponseHeaders 复制响应头部
func (rp *RegistryProxy) copyResponseHeaders(src *http.Response, c *gin.Context) {
	for key, values := range src.Header {
		for _, value := range values {
			c.Header(key, value)
		}
	}
}

// setUpstreamAuth 设置上游认证头部 - 支持透传模式
func (rp *RegistryProxy) setUpstreamAuth(req *http.Request, upstream *UpstreamConfig) {
	if upstream.Auth != nil {
		authConfig, err := upstream.Auth.Authorization()
		if err == nil && authConfig != nil {
			// 优先使用 RegistryToken (Bearer Token)
			if authConfig.RegistryToken != "" {
				req.Header.Set("Authorization", "Bearer "+authConfig.RegistryToken)
			} else if authConfig.Auth != "" {
				// 使用 Auth 字段 (Base64 编码的 username:password)
				req.Header.Set("Authorization", "Basic "+authConfig.Auth)
			} else if authConfig.Username != "" && authConfig.Password != "" {
				// 使用基本认证
				req.SetBasicAuth(authConfig.Username, authConfig.Password)
			} else if authConfig.IdentityToken != "" {
				// 使用身份令牌
				req.Header.Set("Authorization", "Bearer "+authConfig.IdentityToken)
			}
		}
	}
}

// setUpstreamAuthWithClient 统一认证处理 - 避免重复设置
func (rp *RegistryProxy) setUpstreamAuthWithClient(req *http.Request, upstream *UpstreamConfig, c *gin.Context) {
	// 🔥 检查是否已经设置了Authorization头部（通过copyHeaders设置）
	if req.Header.Get("Authorization") != "" {
		rp.logger.Debug("Authorization already set via copyHeaders")
		return
	}
	
	// 如果没有Authorization头部，使用配置的上游认证（主要用于内部健康检查等）
	rp.setUpstreamAuth(req, upstream)
}

// getDefaultUpstream 获取默认上游配置
func (rp *RegistryProxy) getDefaultUpstream() *UpstreamConfig {
	for _, upstream := range rp.upstreams {
		return upstream // 返回第一个配置的上游
	}
	return nil
}

// extractUUIDFromLocation 从Location头部提取UUID
func (rp *RegistryProxy) extractUUIDFromLocation(location string) string {
	// 解析Location头部，提取UUID
	parts := strings.Split(location, "/")
	if len(parts) > 0 {
		uuid := parts[len(parts)-1]
		if len(uuid) == 36 { // 假设UUID格式为36个字符
			return uuid
		}
	}
	return ""
} 