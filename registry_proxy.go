package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.uber.org/zap"
)

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
	Port            int           `json:"port"`
	TLSCert         string        `json:"tls_cert"`
	TLSKey          string        `json:"tls_key"`
	TokenExpiry     time.Duration `json:"token_expiry"`
	MaxConcurrent   int           `json:"max_concurrent"`
	BufferSize      int           `json:"buffer_size"`
	EnableMetrics   bool          `json:"enable_metrics"`
	LogLevel        string        `json:"log_level"`
	DataPersistence bool          `json:"data_persistence"`
	DataFile        string        `json:"data_file"`
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
}

// 注意：AuthService 已被 MemoryAuthService 替代

// ProxyService 代理服务
type ProxyService struct {
	upstreams      map[string]*UpstreamConfig
	logger         *zap.Logger
	semaphore      chan struct{} // 控制并发
	bufferSize     int
	uploadSessions map[string]*UploadSessionManager
	sessionMutex   sync.RWMutex
}

// UploadSessionManager 上传会话管理器
type UploadSessionManager struct {
	session    *UploadSession
	upstream   *UpstreamConfig
	pipeReader *io.PipeReader
	pipeWriter *io.PipeWriter
	ctx        context.Context
	cancel     context.CancelFunc
	errChan    chan error
	doneChan   chan struct{}
	mutex      sync.RWMutex
}

// MemoryAuthService 基于内存的认证服务
type MemoryAuthService struct {
	store         *MemoryStore
	tokenExpiry   time.Duration
	logger        *zap.Logger
	secret        []byte
	jwtSigningKey []byte
}

// TokenResponse Docker认证令牌响应
type TokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// ManifestPushRequest 推送请求
type ManifestPushRequest struct {
	Repository string
	Tag        string
	Manifest   []byte
	MediaType  string
}

// NewRegistryProxy 创建新的注册表代理
func NewRegistryProxy(config *Config) (*RegistryProxy, error) {
	logger, _ := zap.NewProduction()
	
	// 初始化内存存储
	store := NewMemoryStore()
	
	// 生成随机密钥
	secret := make([]byte, 32)
	rand.Read(secret)
	
	// 生成JWT签名密钥
	jwtKey := make([]byte, 32)
	rand.Read(jwtKey)
	
	// 初始化认证服务
	authService := &MemoryAuthService{
		store:         store,
		tokenExpiry:   config.TokenExpiry,
		logger:        logger,
		secret:        secret,
		jwtSigningKey: jwtKey,
	}
	
	// 初始化代理服务
	proxyService := &ProxyService{
		upstreams:      make(map[string]*UpstreamConfig),
		logger:         logger,
		semaphore:      make(chan struct{}, config.MaxConcurrent),
		bufferSize:     config.BufferSize,
		uploadSessions: make(map[string]*UploadSessionManager),
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
	
	// 创建传输层
	config.Transport = &http.Transport{
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: 10,
	}
	
	// 创建推送器
	pusher, err := remote.NewPusher(
		remote.WithAuth(config.Auth),
		remote.WithTransport(config.Transport),
		remote.WithJobs(4),
	)
	if err != nil {
		return fmt.Errorf("failed to create pusher: %w", err)
	}
	config.Pusher = pusher
	
	rp.upstreams[config.Name] = config
	rp.proxyService.upstreams[config.Name] = config
	
	rp.logger.Info("Added upstream registry", 
		zap.String("name", config.Name),
		zap.String("registry", config.Registry))
	
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

// HandleAuth 处理Docker认证
func (rp *RegistryProxy) HandleAuth(c *gin.Context) {
	service := c.Query("service")
	scope := c.Query("scope")
	
	// 验证基本认证
	username, password, ok := c.Request.BasicAuth()
	if !ok {
		c.Header("WWW-Authenticate", `Basic realm="Registry Realm"`)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	
	// 验证用户凭据（这里可以集成企业认证系统）
	if !rp.authService.ValidateCredentials(username, password) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}
	
	// 生成访问令牌
	token, err := rp.authService.GenerateToken(username, service, scope)
	if err != nil {
		rp.logger.Error("Failed to generate token", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token generation failed"})
		return
	}
	
	response := TokenResponse{
		Token:       token,
		AccessToken: token,
		ExpiresIn:   int(rp.config.TokenExpiry.Seconds()),
	}
	
	c.JSON(http.StatusOK, response)
}

// HandleManifestPut 处理Manifest推送
func (rp *RegistryProxy) HandleManifestPut(c *gin.Context) {
	repository := c.Param("name")
	reference := c.Param("reference")
	
	// 读取manifest数据
	_, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read manifest"})
		return
	}
	
	mediaType := c.GetHeader("Content-Type")
	if mediaType == "" {
		mediaType = "application/vnd.docker.distribution.manifest.v2+json"
	}
	
	// 解析目标注册表
	upstream := rp.determineUpstream(repository)
	if upstream == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upstream configured"})
		return
	}
	
	// TODO: 实现异步推送到上游
	rp.logger.Info("Manifest push queued", 
		zap.String("repository", repository), 
		zap.String("reference", reference),
		zap.String("mediaType", mediaType),
		zap.String("upstream", upstream.Registry))
	
	// 立即响应客户端
	c.Header("Location", fmt.Sprintf("/v2/%s/manifests/%s", repository, reference))
	c.Status(http.StatusCreated)
}

// HandleBlobUploadStart 开始blob上传
func (rp *RegistryProxy) HandleBlobUploadStart(c *gin.Context) {
	repository := c.Param("name")
	
	// 生成上传UUID
	uploadUUID := rp.generateUploadUUID()
	
	// 创建上传会话
	session := &UploadSession{
		UUID:       uploadUUID,
		Repository: repository,
		StartTime:  time.Now(),
		Buffer:     make([]byte, 0, rp.config.BufferSize),
	}
	
	// 缓存会话信息
	if err := rp.store.SetUploadSession(uploadUUID, session); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upload session"})
		return
	}
	
	location := fmt.Sprintf("/v2/%s/blobs/uploads/%s", repository, uploadUUID)
	c.Header("Location", location)
	c.Header("Range", "0-0")
	c.Status(http.StatusAccepted)
}

// HandleBlobUploadChunk 处理分块上传
func (rp *RegistryProxy) HandleBlobUploadChunk(c *gin.Context) {
	repository := c.Param("name")
	uploadUUID := c.Param("uuid")
	
	// 获取上传会话
	session, err := rp.store.GetUploadSession(uploadUUID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}
	
	// 读取数据块
	chunk, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read chunk"})
		return
	}
	
	// TODO: 实现流式写入上游（避免内存积累）
	upstream := rp.determineUpstream(repository)
	if upstream != nil {
		rp.logger.Debug("Chunk received for streaming", 
			zap.String("repository", repository),
			zap.String("uploadUUID", uploadUUID),
			zap.Int("chunkSize", len(chunk)))
	}
	
	// 更新会话状态
	session.Offset += int64(len(chunk))
	rp.store.SetUploadSession(uploadUUID, session)
	
	location := fmt.Sprintf("/v2/%s/blobs/uploads/%s", repository, uploadUUID)
	c.Header("Location", location)
	c.Header("Range", fmt.Sprintf("0-%d", session.Offset-1))
	c.Status(http.StatusAccepted)
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
		
		c.Next()
	}
}

// 辅助方法
func (rp *RegistryProxy) determineUpstream(repository string) *UpstreamConfig {
	// 可以基于repository名称、配置规则等确定上游
	// 这里简化为使用第一个配置的上游
	for _, upstream := range rp.upstreams {
		return upstream
	}
	return nil
}

func (rp *RegistryProxy) generateUploadUUID() string {
	// 生成UUID的实现
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// UploadSession 上传会话
type UploadSession struct {
	UUID       string    `json:"uuid"`
	Repository string    `json:"repository"`
	StartTime  time.Time `json:"start_time"`
	Offset     int64     `json:"offset"`
	Buffer     []byte    `json:"-"`
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

// 占位符方法声明
func (rp *RegistryProxy) HandleManifestGet(c *gin.Context)     {}
func (rp *RegistryProxy) HandleManifestDelete(c *gin.Context)  {}
func (rp *RegistryProxy) HandleManifestHead(c *gin.Context)    {}
func (rp *RegistryProxy) HandleBlobGet(c *gin.Context)         {}
func (rp *RegistryProxy) HandleBlobHead(c *gin.Context)        {}
func (rp *RegistryProxy) HandleBlobDelete(c *gin.Context)      {}
func (rp *RegistryProxy) HandleBlobUploadComplete(c *gin.Context) {}
func (rp *RegistryProxy) HandleBlobUploadCancel(c *gin.Context)   {}
func (rp *RegistryProxy) HandleBlobUploadStatus(c *gin.Context)   {}
func (rp *RegistryProxy) HandleCatalog(c *gin.Context)            {}
func (rp *RegistryProxy) HandleTagsList(c *gin.Context)           {} 