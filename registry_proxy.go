package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.uber.org/zap"
)

// UUID正则表达式，用于验证标准UUID格式（RFC 4122）
var uuidRegex = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

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
	upstreams    []*UpstreamConfig
	upstreamMap  map[string]*UpstreamConfig
	logger       *zap.Logger
	config       *Config
}

// Config 服务配置
type Config struct {
	Port               int                        `json:"port"`
	TLSCert            string                     `json:"tls_cert"`
	TLSKey             string                     `json:"tls_key"`
	TokenExpiry        Duration                   `json:"token_expiry"`
	MaxConcurrent      int                        `json:"max_concurrent"`
	BufferSize         int                        `json:"buffer_size"`
	LogLevel           string                     `json:"log_level"`
	DataPersistence    bool                       `json:"data_persistence"`
	DataFile           string                     `json:"data_file"`
	ConnectionPoolSize int                        `json:"connection_pool_size"`
	RequestTimeout     Duration                   `json:"request_timeout"`
	Upstreams          []*UpstreamConfigJSON      `json:"upstreams"`
}

// UpstreamConfigJSON 用于JSON配置的上游配置（不包含运行时字段）
type UpstreamConfigJSON struct {
	Name         string `json:"name"`
	Registry     string `json:"registry"`
	RoutePattern string `json:"route_pattern"`
}

// UpstreamConfig 上游注册表配置
type UpstreamConfig struct {
	Name         string             `json:"name"`
	Registry     string             `json:"registry"`
	Transport    http.RoundTripper  `json:"-"`
	Pusher       *remote.Pusher     `json:"-"`
	RoutePattern string             `json:"route_pattern"`
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

func (usm *UploadSessionManager) SafeUpdateOffset(delta int64) {
	usm.mutex.Lock()
	defer usm.mutex.Unlock()
	usm.session.Offset += delta
}

type MemoryAuthService struct {
	store         *MemoryStore
	tokenExpiry   time.Duration
	logger        *zap.Logger
	jwtSigningKey []byte
}

type TokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

func NewRegistryProxy(config *Config) (*RegistryProxy, error) {
	logger, _ := zap.NewProduction()
	
	store := NewMemoryStore()
	
	jwtKey := make([]byte, 32)
	cryptorand.Read(jwtKey)
	
	authService := &MemoryAuthService{
		store:         store,
		tokenExpiry:   config.TokenExpiry.ToDuration(),
		logger:        logger,
		jwtSigningKey: jwtKey,
	}
	
	proxyService := &ProxyService{
		logger:         logger,
		semaphore:      make(chan struct{}, config.MaxConcurrent),
		bufferSize:     config.BufferSize,
		uploadSessions: make([]*sessionShard, config.ConnectionPoolSize),
		shardCount:     config.ConnectionPoolSize,
	}
	
	for i := 0; i < config.ConnectionPoolSize; i++ {
		proxyService.uploadSessions[i] = &sessionShard{
			sessions: make(map[string]*UploadSessionManager),
		}
	}
	
	proxy := &RegistryProxy{
		authService:  authService,
		proxyService: proxyService,
		store:        store,
		upstreams:    make([]*UpstreamConfig, 0),
		upstreamMap:  make(map[string]*UpstreamConfig),
		logger:       logger,
		config:       config,
	}
	
	if config.DataPersistence && config.DataFile != "" {
		proxy.loadPersistedData()
	}
	
	return proxy, nil
}

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

func (rp *RegistryProxy) AddUpstream(config *UpstreamConfig) error {
	config.Transport = &http.Transport{
		MaxIdleConns:           200,
		MaxIdleConnsPerHost:    50,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  30 * time.Second,
		ExpectContinueTimeout:  1 * time.Second,
		MaxConnsPerHost:        100,
		DisableCompression:     true,
		WriteBufferSize:        rp.config.BufferSize,
		ReadBufferSize:         rp.config.BufferSize,
	}
	
	pusher, err := remote.NewPusher(
		remote.WithTransport(config.Transport),
		remote.WithJobs(rp.config.MaxConcurrent/2),
	)
	if err != nil {
		return fmt.Errorf("failed to create pusher: %w", err)
	}
	config.Pusher = pusher
	
	rp.upstreams = append(rp.upstreams, config)
	rp.upstreamMap[config.Name] = config
	
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
	
	// Docker Registry API V2 路由 - 使用单一通配符路由处理所有请求
	v2 := router.Group("/v2")
	{
		// 使用单一通配符路由处理所有请求，在应用层进行认证和分发
		v2.GET("/*path", rp.HandleUnifiedRequest)
		v2.PUT("/*path", rp.HandleUnifiedRequest)
		v2.POST("/*path", rp.HandleUnifiedRequest)
		v2.DELETE("/*path", rp.HandleUnifiedRequest)
		v2.HEAD("/*path", rp.HandleUnifiedRequest)
		v2.PATCH("/*path", rp.HandleUnifiedRequest)
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

// HandleUnifiedRequest 统一处理所有Docker Registry请求，包含认证逻辑
func (rp *RegistryProxy) HandleUnifiedRequest(c *gin.Context) {
	path := strings.TrimPrefix(c.Param("path"), "/")
	method := c.Request.Method
	
	// 特殊端点：ping端点（空路径）不需要认证
	if path == "" && method == "GET" {
		rp.HandlePing(c)
		return
	}
	
	// 特殊端点：认证端点不需要认证
	if path == "auth" {
		rp.HandleAuth(c)
		return
	}
	
	// 特殊端点：_catalog
	if path == "_catalog" && method == "GET" {
		// 需要认证
		if !rp.checkAuthentication(c) {
			return
		}
		rp.HandleCatalog(c)
		return
	}
	
	// 其他所有端点都需要认证
	if !rp.checkAuthentication(c) {
		return
	}
	
	// 解析路径并分发到对应的Handler（按优先级排序，避免误匹配）
	switch {
	case strings.Contains(path, "/blobs/uploads/"):
		rp.handleBlobUploadRequest(c, path, method)
	case strings.Contains(path, "/manifests/"):
		rp.handleManifestRequest(c, path, method)
	case strings.Contains(path, "/blobs/"):
		rp.handleBlobRequest(c, path, method)
	case strings.Contains(path, "/tags/list") && strings.HasSuffix(path, "/tags/list"):
		if method == "GET" {
			// 设置仓库名参数
			repository := strings.TrimSuffix(path, "/tags/list")
			if repository == "" {
				c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
				return
			}
			c.Set("repository", repository)
			rp.HandleTagsList(c)
		} else {
			c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "method not allowed"})
		}
	default:
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	}
}

// checkAuthentication 检查认证，返回true表示认证通过
func (rp *RegistryProxy) checkAuthentication(c *gin.Context) bool {
	authHeader := c.GetHeader("Authorization")
	if authHeader == "" {
		// 构建完整的认证URL - 在反向代理环境下正确判断协议
		scheme := "https"
		// 检查多种可能的HTTPS指示器
		if c.Request.TLS == nil && 
		   c.GetHeader("X-Forwarded-Proto") != "https" && 
		   c.GetHeader("X-Forwarded-Ssl") != "on" &&
		   c.GetHeader("X-Scheme") != "https" {
			scheme = "http"
		}
		host := c.Request.Host
		authURL := fmt.Sprintf("%s://%s/v2/auth", scheme, host)
		
			// 从请求路径推断scope
	path := strings.TrimPrefix(c.Param("path"), "/")
	scope := rp.inferScopeFromPath(path, c.Request.Method)
	
	// 使用标准的service参数
	service := "registry.docker.io"
	
	// 根据Docker Hub的行为，某些请求不包含scope
	// 特别是POST /blobs/uploads/ 请求
	var wwwAuth string
	if scope != "" && !rp.shouldOmitScope(path, c.Request.Method) {
		wwwAuth = fmt.Sprintf(`Bearer realm="%s",service="%s",scope="%s"`, authURL, service, scope)
	} else {
		wwwAuth = fmt.Sprintf(`Bearer realm="%s",service="%s",scope=""`, authURL, service)
	}
		
		c.Header("WWW-Authenticate", wwwAuth)
		c.Header("Docker-Distribution-API-Version", "registry/2.0")
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return false
	}
	
	// 验证Bearer Token
	if !strings.HasPrefix(authHeader, "Bearer ") {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid auth format"})
		return false
	}
	
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if !rp.authService.ValidateToken(token) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
		return false
	}
	
	// 将token存储到上下文中，供后续透传使用
	c.Set("auth_token", token)
	return true
}

// handleManifestRequest 处理Manifest相关请求
func (rp *RegistryProxy) handleManifestRequest(c *gin.Context, path, method string) {
	parts := strings.Split(path, "/manifests/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	
	// 设置参数供Handler使用
	c.Set("repository", parts[0])
	c.Set("reference", parts[1])
	
	switch method {
	case "GET":
		rp.HandleManifestGet(c)
	case "PUT":
		rp.HandleManifestPut(c)
	case "DELETE":
		rp.HandleManifestDelete(c)
	case "HEAD":
		rp.HandleManifestHead(c)
	default:
		c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "method not allowed"})
	}
}

// handleBlobRequest 处理Blob相关请求
func (rp *RegistryProxy) handleBlobRequest(c *gin.Context, path, method string) {
	parts := strings.Split(path, "/blobs/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	
	// 设置参数供Handler使用
	c.Set("repository", parts[0])
	c.Set("digest", parts[1])
	
	switch method {
	case "GET":
		rp.HandleBlobGet(c)
	case "HEAD":
		rp.HandleBlobHead(c)
	case "DELETE":
		rp.HandleBlobDelete(c)
	default:
		c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "method not allowed"})
	}
}

// handleBlobUploadRequest 处理Blob上传相关请求
func (rp *RegistryProxy) handleBlobUploadRequest(c *gin.Context, path, method string) {
	parts := strings.Split(path, "/blobs/uploads/")
	if len(parts) != 2 || parts[0] == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	
	repository := parts[0]
	uploadPath := parts[1]
	
	// 设置参数供Handler使用
	c.Set("repository", repository)
	
	if uploadPath == "" {
		// POST /v2/{name}/blobs/uploads/
		if method == "POST" {
			rp.HandleBlobUploadStart(c)
		} else {
			c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "method not allowed"})
		}
	} else {
		// 包含UUID的上传操作 - 提取并验证UUID
		uuid := strings.Split(uploadPath, "/")[0] // 只取第一段作为UUID，忽略额外路径
		if uuid == "" {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.Set("uuid", uuid)
		switch method {
		case "PATCH":
			rp.HandleBlobUploadChunk(c)
		case "PUT":
			rp.HandleBlobUploadComplete(c)
		case "DELETE":
			rp.HandleBlobUploadCancel(c)
		case "GET":
			rp.HandleBlobUploadStatus(c)
		default:
			c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "method not allowed"})
		}
	}
}

// getRepository 统一获取仓库名，兼容新的参数设置方式
func (rp *RegistryProxy) getRepository(c *gin.Context) string {
	// 优先从Context获取（新方式）
	if repo := c.GetString("repository"); repo != "" {
		return repo
	}
	
	// 兼容原有方式
	name := c.Param("name")
	if strings.HasPrefix(name, "/") {
		return strings.TrimPrefix(name, "/")
	}
	return name
}

// getReference 获取引用（标签或摘要）
func (rp *RegistryProxy) getReference(c *gin.Context) string {
	// 优先从Context获取（新方式）
	if ref := c.GetString("reference"); ref != "" {
		return ref
	}
	// 兼容原有方式
	return c.Param("reference")
}

// getDigest 获取摘要
func (rp *RegistryProxy) getDigest(c *gin.Context) string {
	// 优先从Context获取（新方式）
	if digest := c.GetString("digest"); digest != "" {
		return digest
	}
	// 兼容原有方式
	return c.Param("digest")
}

// getUUID 获取上传UUID
func (rp *RegistryProxy) getUUID(c *gin.Context) string {
	// 优先从Context获取（新方式）
	if uuid := c.GetString("uuid"); uuid != "" {
		return uuid
	}
	// 兼容原有方式
	return c.Param("uuid")
}

func (rp *RegistryProxy) HandleAuth(c *gin.Context) {
	service := c.Query("service")
	scope := c.Query("scope")
	
	// Docker客户端可能不发送Basic Auth，支持匿名token请求
	username, password, hasBasicAuth := c.Request.BasicAuth()
	
	// 如果没有Basic Auth，使用默认匿名用户
	if !hasBasicAuth || username == "" {
		username = "anonymous"
		password = ""
	}
	
	rp.logger.Info("Client authentication request", 
		zap.String("username", username),
		zap.String("service", service),
		zap.String("scope", scope))
	
	token, err := rp.authService.GenerateToken(username, password, service, scope)
	if err != nil {
		rp.logger.Error("Failed to generate token", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token generation failed"})
		return
	}
	
	response := TokenResponse{
		Token:       token,
		AccessToken: token,
		ExpiresIn:   int(rp.config.TokenExpiry.ToDuration().Seconds()),
	}
	
	c.JSON(http.StatusOK, response)
}

func (rp *RegistryProxy) HandleManifestPut(c *gin.Context) {
	// 非阻塞信号量控制
	select {
	case rp.proxyService.semaphore <- struct{}{}:
		defer func() { <-rp.proxyService.semaphore }()
	default:
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "server busy, please retry later"})
		return
	}
	
	repository := rp.getRepository(c)
	reference := rp.getReference(c)
	
	upstream := rp.determineUpstream(repository)
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", upstream.Registry, repository, reference)
	
	req, err := http.NewRequestWithContext(c.Request.Context(), "PUT", upstreamURL, c.Request.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upstream request"})
		return
	}
	
	rp.copyHeaders(c.Request, req, []string{"Content-Type", "Content-Length", "Authorization"})
	
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
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

func (rp *RegistryProxy) HandleBlobUploadStart(c *gin.Context) {
	repository := rp.getRepository(c)
	
	upstream := rp.determineUpstream(repository)
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/blobs/uploads/", upstream.Registry, repository)
	
	req, err := http.NewRequestWithContext(c.Request.Context(), "POST", upstreamURL, nil)
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
	
	location := resp.Header.Get("Location")
	if location == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream did not provide upload location"})
		return
	}
	
	uploadUUID := rp.extractUUIDFromLocation(location)
	if uploadUUID == "" {
		uploadUUID = rp.generateUploadUUID()
		rp.logger.Warn("Failed to extract UUID from location, generated new one",
			zap.String("location", location),
			zap.String("generated_uuid", uploadUUID))
	}
	
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
	
	rp.proxyService.SetUploadSession(uploadUUID, sessionManager)
	rp.store.SetUploadSession(uploadUUID, sessionManager.session)
	
	proxyLocation := fmt.Sprintf("/v2/%s/blobs/uploads/%s", repository, uploadUUID)
	
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
	// 非阻塞信号量控制
	select {
	case rp.proxyService.semaphore <- struct{}{}:
		defer func() { <-rp.proxyService.semaphore }()
	default:
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "server busy, please retry later"})
		return
	}
	
	repository := rp.getRepository(c)
	uploadUUID := rp.getUUID(c)
	
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
// AuthMiddleware 已移除，认证逻辑集成到HandleUnifiedRequest中

// determineUpstream 智能上游路由选择
func (rp *RegistryProxy) determineUpstream(repository string) *UpstreamConfig {

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

// 其他处理函数的声明
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
	repository := rp.getRepository(c)
	reference := rp.getReference(c)
	
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
	repository := rp.getRepository(c)
	reference := rp.getReference(c)
	
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
	repository := rp.getRepository(c)
	reference := rp.getReference(c)
	
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
	repository := rp.getRepository(c)
	digest := rp.getDigest(c)
	
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
	repository := rp.getRepository(c)
	digest := rp.getDigest(c)
	
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
	repository := rp.getRepository(c)
	digest := rp.getDigest(c)
	
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
	// 非阻塞信号量控制
	select {
	case rp.proxyService.semaphore <- struct{}{}:
		defer func() { <-rp.proxyService.semaphore }()
	default:
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "server busy, please retry later"})
		return
	}
	
	repository := rp.getRepository(c)
	uploadUUID := rp.getUUID(c)
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
	
	// 使用defer确保无论成功失败都清理会话
	defer func() {
		sessionManager.Close()
		rp.proxyService.DeleteUploadSession(uploadUUID)
		rp.store.DeleteUploadSession(uploadUUID)
	}()
	
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
	
	// 转发响应
	rp.copyResponseHeaders(resp, c)
	c.Status(resp.StatusCode)
}

// HandleBlobUploadCancel 取消上传
func (rp *RegistryProxy) HandleBlobUploadCancel(c *gin.Context) {
	repository := rp.getRepository(c)
	uploadUUID := rp.getUUID(c)
	
	upstream := rp.determineUpstream(repository)
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	// 清理本地会话
	sessionManager, exists := rp.proxyService.GetUploadSession(uploadUUID)
	if exists {
		sessionManager.Close()
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
	repository := rp.getRepository(c)
	uploadUUID := rp.getUUID(c)
	
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
	repository := rp.getRepository(c)
	
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

// setUpstreamAuthWithClient 统一认证处理 - 透传模式
func (rp *RegistryProxy) setUpstreamAuthWithClient(req *http.Request, upstream *UpstreamConfig, c *gin.Context) {
	// 优先级1：直接透传Authorization头部
	if req.Header.Get("Authorization") != "" {
		rp.logger.Debug("Authorization already set via copyHeaders")
		return
	}
	
	// 优先级2：JWT解析重建认证
	if authToken, exists := c.Get("auth_token"); exists {
		if claims, err := rp.authService.ParseToken(authToken.(string)); err == nil {
			if claims.ClientAuth != "" {
				if authData, err := base64.StdEncoding.DecodeString(claims.ClientAuth); err == nil {
					parts := strings.Split(string(authData), ":")
					if len(parts) == 2 {
						req.SetBasicAuth(parts[0], parts[1])
						rp.logger.Debug("Using JWT embedded client auth", zap.String("username", claims.Username))
						return
					}
				}
			}
		}
	}
	
	// 上游处理认证失败
	rp.logger.Debug("No valid auth found, letting upstream handle authentication")
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
	// 移除查询参数
	if idx := strings.Index(location, "?"); idx != -1 {
		location = location[:idx]
	}
	
	// 使用正则表达式匹配标准UUID格式
	return uuidRegex.FindString(location)
}

// shouldOmitScope 判断是否应该省略scope参数（模拟Docker Hub行为）
func (rp *RegistryProxy) shouldOmitScope(path, method string) bool {
	// 根据Docker Hub的行为，POST /blobs/uploads/ 请求返回空scope
	if method == "POST" && strings.Contains(path, "/blobs/uploads/") {
		return true
	}
	return false
}

// inferScopeFromPath 从请求路径推断scope参数
func (rp *RegistryProxy) inferScopeFromPath(path, method string) string {
	if path == "" || path == "_catalog" {
		return ""
	}
	
	// 调试日志
	rp.logger.Debug("Inferring scope", zap.String("path", path), zap.String("method", method))
	
	// 解析仓库名
	var repository string
	var actions []string
	
	switch {
	case strings.Contains(path, "/manifests/"):
		parts := strings.Split(path, "/manifests/")
		if len(parts) == 2 && parts[0] != "" {
			repository = parts[0]
			switch method {
			case "GET", "HEAD":
				actions = []string{"pull"}
			case "PUT":
				actions = []string{"push", "pull"}
			case "DELETE":
				actions = []string{"delete"}
			}
		}
	case strings.Contains(path, "/blobs/uploads/"):
		// 必须在/blobs/之前检查，因为/blobs/uploads/包含/blobs/
		parts := strings.Split(path, "/blobs/uploads/")
		if len(parts) == 2 && parts[0] != "" {
			repository = parts[0]
			// 修正：上传操作需要push和pull权限
			if method == "POST" {
				actions = []string{"push", "pull"}
			} else {
				actions = []string{"pull"}
			}
		}
	case strings.Contains(path, "/blobs/"):
		parts := strings.Split(path, "/blobs/")
		if len(parts) == 2 && parts[0] != "" {
			repository = parts[0]
			switch method {
			case "GET", "HEAD":
				actions = []string{"pull"}
			case "DELETE":
				actions = []string{"delete"}
			}
		}
	case strings.HasSuffix(path, "/tags/list"):
		repository = strings.TrimSuffix(path, "/tags/list")
		if repository != "" {
			actions = []string{"pull"}
		}
	}
	
	if repository != "" && len(actions) > 0 {
		scope := fmt.Sprintf("repository:%s:%s", repository, strings.Join(actions, ","))
		rp.logger.Debug("Generated scope", zap.String("scope", scope))
		return scope
	}
	
	rp.logger.Debug("No scope generated", zap.String("repository", repository), zap.Strings("actions", actions))
	return ""
} 