package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/vulcand/oxy/v2/forward"
	"golang.org/x/time/rate"
)

const (
	UPSTREAM_REGISTRY_HOST = "registry-1.docker.io"
	CleanupInterval = 10 * time.Minute
	MaxIPCacheSize = 10000
)

var (
	realmRegex   = regexp.MustCompile(`realm="(.*?)"`)
	serviceRegex = regexp.MustCompile(`service="(.*?)"`)
	scopeRegex   = regexp.MustCompile(`scope="(.*?)"`)
	locationRegex = regexp.MustCompile(`^https?://[^/]+(.*)$`)
	
	externalCDNPatterns = []*regexp.Regexp{
		regexp.MustCompile(`.*\.cloudflare\.docker\.com$`),
		regexp.MustCompile(`.*\.docker\.com$`),
		regexp.MustCompile(`.*\.cloudfront\.net$`),
		regexp.MustCompile(`.*cdn.*\.(com|net|org)$`),
	}
)

var streamingProxy http.Handler
var globalLimiter *IPRateLimiter

var sharedHTTPClient *http.Client
var sharedCDNClient *http.Client

type Config struct {
	Server    ServerConfig    `json:"server"`
	RateLimit RateLimitConfig `json:"rateLimit"`
	Security  SecurityConfig  `json:"security"`
	Timeouts  TimeoutConfig   `json:"timeouts"`
}

type ServerConfig struct {
	Port int    `json:"port"`
	Host string `json:"host"`
}

type RateLimitConfig struct {
	RequestLimit int     `json:"requestLimit"`
	PeriodHours  float64 `json:"periodHours"`
}

type SecurityConfig struct {
	WhiteList []string `json:"whiteList"`
	BlackList []string `json:"blackList"`
}

type TimeoutConfig struct {
	ReadTimeout       int `json:"readTimeout"`       // 读取超时(秒)
	WriteTimeout      int `json:"writeTimeout"`      // 写入超时(秒)
	ReadHeaderTimeout int `json:"readHeaderTimeout"` // 读取头部超时(秒)
	IdleTimeout       int `json:"idleTimeout"`       // 空闲超时(秒)
	ClientTimeout     int `json:"clientTimeout"`     // 客户端超时(秒)
	CDNTimeout        int `json:"cdnTimeout"`        // CDN超时(秒)
}

type IPRateLimiter struct {
	ips       map[string]*rateLimiterEntry
	mu        *sync.RWMutex
	r         rate.Limit
	b         int
	whitelist []*net.IPNet
	blacklist []*net.IPNet
}

type rateLimiterEntry struct {
	limiter    *rate.Limiter
	lastAccess time.Time
}

func GetConfig() *Config {
	config := &Config{
		Server: ServerConfig{
			Port: 5002,
			Host: "",
		},
		RateLimit: RateLimitConfig{
			RequestLimit: 200,
			PeriodHours:  1.0,
		},
		Security: SecurityConfig{
			WhiteList: []string{
				"127.0.0.1/32",
				"::1/128",
			},
			BlackList: []string{},
		},
		Timeouts: TimeoutConfig{
			ReadTimeout:       600,  // 10分钟，支持大镜像读取
			WriteTimeout:      600,  // 10分钟，支持大镜像写入
			ReadHeaderTimeout: 30,   // 30秒，防止慢速攻击
			IdleTimeout:       300,  // 5分钟，及时释放空闲连接
			ClientTimeout:     600,  // 10分钟，客户端请求超时
			CDNTimeout:        600,  // 10分钟，CDN超时
		},
	}
	
	// 优先使用环境变量指定的配置文件，否则尝试加载当前目录的config.json
	configFile := os.Getenv("CONFIG_FILE")
	if configFile == "" {
		configFile = "config.json"
	}
	
	if data, err := os.ReadFile(configFile); err == nil {
		json.Unmarshal(data, config)
	}
	
	return config
}

func extractIPFromAddress(address string) string {
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return address
}

func normalizeIPForRateLimit(ipStr string) string {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ipStr
	}
	
	if ip.To4() != nil {
		return ipStr
	}
	
	// IPv6标准化为/64网段
	ipv6 := make([]byte, 16)
	copy(ipv6, ip.To16())
	for i := 8; i < 16; i++ {
		ipv6[i] = 0
	}
	return net.IP(ipv6).String() + "/64"
}

func isIPInCIDRList(ip string, cidrList []*net.IPNet) bool {
	cleanIP := extractIPFromAddress(ip)
	parsedIP := net.ParseIP(cleanIP)
	if parsedIP == nil {
		return false
	}
	
	for _, cidr := range cidrList {
		if cidr.Contains(parsedIP) {
			return true
		}
	}
	return false
}

func (i *IPRateLimiter) cleanupRoutine() {
	ticker := time.NewTicker(CleanupInterval)
	defer ticker.Stop()
	
	for range ticker.C {
		now := time.Now()
		expired := make([]string, 0)
		
		i.mu.RLock()
		for ip, entry := range i.ips {
			if now.Sub(entry.lastAccess) > 1*time.Hour {
				expired = append(expired, ip)
			}
		}
		i.mu.RUnlock()
		
		if len(expired) > 0 || len(i.ips) > MaxIPCacheSize {
			i.mu.Lock()
			for _, ip := range expired {
				delete(i.ips, ip)
			}
			
			if len(i.ips) > MaxIPCacheSize {
				i.ips = make(map[string]*rateLimiterEntry)
			}
			i.mu.Unlock()
		}
	}
}

func (i *IPRateLimiter) GetLimiter(ip string) (*rate.Limiter, bool) {
	cleanIP := extractIPFromAddress(ip)
	
	if isIPInCIDRList(cleanIP, i.blacklist) {
		log.Printf("[RateLimit] 🚫 黑名单IP拒绝: %s", cleanIP)
		return nil, false
	}
	
	if isIPInCIDRList(cleanIP, i.whitelist) {
		log.Printf("[RateLimit] ✅ 白名单IP通过: %s", cleanIP)
		return rate.NewLimiter(rate.Inf, i.b), true
	}
	
	normalizedIP := normalizeIPForRateLimit(cleanIP)
	now := time.Now()
	
	// 使用写锁保护整个操作，消除竞争条件
	i.mu.Lock()
	defer i.mu.Unlock()
	
	// 检查是否已存在限流器
	if entry, exists := i.ips[normalizedIP]; exists {
		entry.lastAccess = now
		return entry.limiter, true
	}
	
	// 创建新的限流器
	entry := &rateLimiterEntry{
		limiter:    rate.NewLimiter(i.r, i.b),
		lastAccess: now,
	}
	i.ips[normalizedIP] = entry
	log.Printf("[RateLimit] 🆕 新IP限流器: %s (标准化: %s)", cleanIP, normalizedIP)
	
	return entry.limiter, true
}

// initGlobalLimiter 初始化全局限流器
func initGlobalLimiter(cfg *Config) *IPRateLimiter {
	
	whitelist := make([]*net.IPNet, 0, len(cfg.Security.WhiteList))
	for _, item := range cfg.Security.WhiteList {
		if item = strings.TrimSpace(item); item != "" {
			if !strings.Contains(item, "/") {
				item = item + "/32" // 单个IP转为CIDR格式
			}
			_, ipnet, err := net.ParseCIDR(item)
			if err == nil {
				whitelist = append(whitelist, ipnet)
			} else {
				log.Printf("警告: 无效的白名单IP格式: %s", item)
			}
		}
	}
	
	// 解析黑名单IP段
	blacklist := make([]*net.IPNet, 0, len(cfg.Security.BlackList))
	for _, item := range cfg.Security.BlackList {
		if item = strings.TrimSpace(item); item != "" {
			if !strings.Contains(item, "/") {
				item = item + "/32" // 单个IP转为CIDR格式
			}
			_, ipnet, err := net.ParseCIDR(item)
			if err == nil {
				blacklist = append(blacklist, ipnet)
			} else {
				log.Printf("警告: 无效的黑名单IP格式: %s", item)
			}
		}
	}
	
	// 计算速率：将 "每N小时X个请求" 转换为 "每秒Y个请求"
	ratePerSecond := rate.Limit(float64(cfg.RateLimit.RequestLimit) / (cfg.RateLimit.PeriodHours * 3600))
	
	burstSize := cfg.RateLimit.RequestLimit
	if burstSize < 1 {
		burstSize = 1
	}
	
	limiter := &IPRateLimiter{
		ips:       make(map[string]*rateLimiterEntry),
		mu:        &sync.RWMutex{},
		r:         ratePerSecond,
		b:         burstSize,
		whitelist: whitelist,
		blacklist: blacklist,
	}
	
	// 启动定期清理goroutine
	go limiter.cleanupRoutine()
	
	return limiter
}

// rateLimitMiddleware 限流中间件
func rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 静态文件豁免：跳过限流检查
		path := r.URL.Path
		if path == "/" || path == "/favicon.ico" {
			next.ServeHTTP(w, r)
			return
		}

		// 获取客户端真实IP
		var ip string
		
		// 优先尝试从请求头获取真实IP
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			// X-Forwarded-For可能包含多个IP，取第一个
			ips := strings.Split(forwarded, ",")
			ip = strings.TrimSpace(ips[0])
		} else if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
			// 如果有X-Real-IP头
			ip = realIP
		} else if remoteIP := r.Header.Get("X-Original-Forwarded-For"); remoteIP != "" {
			// 某些代理可能使用此头
			ips := strings.Split(remoteIP, ",")
			ip = strings.TrimSpace(ips[0])
		} else {
			// 回退到RemoteAddr
			ip = r.RemoteAddr
		}
		
		// 提取纯IP地址（去除可能存在的端口）
		cleanIP := extractIPFromAddress(ip)
		
		// 日志记录请求IP和头信息
		normalizedIP := normalizeIPForRateLimit(cleanIP)
		if cleanIP != normalizedIP {
			log.Printf("[RateLimit] 请求IP: %s (提纯后: %s, 限流段: %s), X-Forwarded-For: %s, X-Real-IP: %s", 
				ip, cleanIP, normalizedIP,
				r.Header.Get("X-Forwarded-For"), 
				r.Header.Get("X-Real-IP"))
		} else {
			log.Printf("[RateLimit] 请求IP: %s (提纯后: %s), X-Forwarded-For: %s, X-Real-IP: %s", 
				ip, cleanIP,
				r.Header.Get("X-Forwarded-For"), 
				r.Header.Get("X-Real-IP"))
		}
		
		// 获取限流器并检查是否允许访问
		ipLimiter, allowed := globalLimiter.GetLimiter(cleanIP)
		
		// 如果IP在黑名单中
		if !allowed {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"您已被限制访问"}`))
			return
		}
		
		// 检查限流
		if !ipLimiter.Allow() {
			log.Printf("[RateLimit] 🚫 IP限流触发: %s", cleanIP)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"请求频率过快，暂时限制访问"}`))
			return
		}
		
		log.Printf("[RateLimit] ✅ IP通过限流: %s → %s %s", cleanIP, r.Method, r.URL.Path)
		
		next.ServeHTTP(w, r)
	})
}

// 获取代理基础URL
func getProxyBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" || r.Header.Get("X-Forwarded-Ssl") == "on" {
		scheme = "https"
	}
	
	host := r.Host
	if host == "" {
		host = r.Header.Get("Host")
	}
	
	return fmt.Sprintf("%s://%s", scheme, host)
}

// 检查是否为外部CDN
func isExternalCDN(urlStr string) bool {
	u, err := url.Parse(urlStr)
	if err != nil {
		return false
	}
	
	hostname := strings.ToLower(u.Host)
	
	if hostname == UPSTREAM_REGISTRY_HOST {
		return false
	}
	
	for _, pattern := range externalCDNPatterns {
		if pattern.MatchString(hostname) {
			// 排除 registry-1.docker.io
			if strings.HasSuffix(hostname, ".docker.com") && hostname == "registry-1.docker.io" {
				continue
			}
			log.Printf("[CDNDetection] 检测到外部CDN: %s", hostname)
			return true
		}
	}
	
	return false
}

// 重写Location头部
func rewriteLocationHeader(originalLocation, proxyBaseURL string) string {
	if originalLocation == "" {
		return ""
	}
	
	u, err := url.Parse(originalLocation)
	if err != nil {
		log.Printf("[LocationRewrite] 无法解析URL: %s", originalLocation)
		return originalLocation
	}
	
	if isExternalCDN(originalLocation) {
		// CDN重定向：保留完整路径和查询参数
		newLocation := fmt.Sprintf("%s%s", proxyBaseURL, u.RequestURI())
		log.Printf("[LocationRewrite] CDN重定向: %s -> %s", originalLocation, newLocation)
		return newLocation
	}
	
	// 普通重定向：提取路径部分
	matches := locationRegex.FindStringSubmatch(originalLocation)
	if len(matches) != 2 {
		return originalLocation
	}
	
	path := matches[1]
	newLocation := proxyBaseURL + path
	
	log.Printf("[LocationRewrite] 普通重定向: %s -> %s", originalLocation, newLocation)
	return newLocation
}

// 创建CDN代理处理器
func createCDNProxy(originalURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[CDNProxy] 代理CDN请求: %s", originalURL)
		
		cdnReq, err := http.NewRequest(r.Method, originalURL, r.Body)
		if err != nil {
			log.Printf("[CDNProxy] 创建CDN请求失败: %v", err)
			http.Error(w, "CDN请求创建失败", http.StatusInternalServerError)
			return
		}
		
		// 复制请求头，排除Host头
		for key, values := range r.Header {
			if strings.ToLower(key) != "host" {
				cdnReq.Header[key] = values
			}
		}
		
		resp, err := sharedCDNClient.Do(cdnReq)
		if err != nil {
			log.Printf("[CDNProxy] CDN请求失败: %v", err)
			http.Error(w, "CDN访问失败", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		
		log.Printf("[CDNProxy] CDN响应状态: %s", resp.Status)
		
		// 复制响应头和状态码
		for key, values := range resp.Header {
			w.Header()[key] = values
		}
		w.WriteHeader(resp.StatusCode)
		
		// 流式复制响应体
		if _, err := io.Copy(w, resp.Body); err != nil {
			log.Printf("[CDNProxy] 复制CDN响应失败: %v", err)
		} else {
			log.Printf("[CDNProxy] CDN请求代理完成")
		}
	}
}

// 响应重写器，处理认证和Location头部
type responseRewriter struct {
	http.ResponseWriter
	proxyBaseURL string
	statusCode   int
}

func (rw *responseRewriter) WriteHeader(code int) {
	rw.statusCode = code
	
	// 处理401认证响应
	if code == http.StatusUnauthorized {
		authHeader := rw.Header().Get("Www-Authenticate")
		if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
			log.Printf("[ResponseRewriter] 收到上游 401, Www-Authenticate: %s", authHeader)

			originalRealm := realmRegex.FindStringSubmatch(authHeader)
			if len(originalRealm) == 2 {
				newRealmURL, _ := url.Parse(rw.proxyBaseURL + "/auth")
				query := newRealmURL.Query()
				query.Set("original_realm", originalRealm[1])

				serviceMatch := serviceRegex.FindStringSubmatch(authHeader)
				if len(serviceMatch) == 2 {
					query.Set("service", serviceMatch[1])
				}
				scopeMatch := scopeRegex.FindStringSubmatch(authHeader)
				if len(scopeMatch) == 2 {
					query.Set("scope", scopeMatch[1])
				}
				newRealmURL.RawQuery = query.Encode()

				var newAuthParts []string
				newAuthParts = append(newAuthParts, fmt.Sprintf(`realm="%s"`, newRealmURL.String()))
				
				if len(serviceMatch) == 2 {
					newAuthParts = append(newAuthParts, fmt.Sprintf(`service="%s"`, serviceMatch[1]))
				}
				if len(scopeMatch) == 2 {
					newAuthParts = append(newAuthParts, fmt.Sprintf(`scope="%s"`, scopeMatch[1]))
				}
				
				newAuthHeader := "Bearer " + strings.Join(newAuthParts, ",")
				rw.Header().Set("Www-Authenticate", newAuthHeader)
				log.Printf("[ResponseRewriter] 修改后 Www-Authenticate: %s", newAuthHeader)
			}
		}
	}

	// 处理Location头部重写
	if location := rw.Header().Get("Location"); location != "" {
		newLocation := rewriteLocationHeader(location, rw.proxyBaseURL)
		rw.Header().Set("Location", newLocation)
	}

	rw.ResponseWriter.WriteHeader(code)
}

// 代理处理器，处理URL重写和请求转发
type proxyHandler struct {
	fwd http.Handler
}

func (p *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 重写请求到上游仓库
	r.URL.Scheme = "https"
	r.URL.Host = UPSTREAM_REGISTRY_HOST
	
	// 保存原始信息用于响应处理
	r.Header.Set("X-Original-Host", r.Host)
	r.Header.Set("X-Original-Scheme", "http")
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		r.Header.Set("X-Original-Scheme", "https")
	}
	
	// 使用forward代理转发
	p.fwd.ServeHTTP(w, r)
}

// 创建代理
func createStreamingProxy() http.Handler {
	// 创建转发器
	fwd := forward.New(false)
	
	// 包装成代理处理器
	return &proxyHandler{fwd: fwd}
}

// 处理认证请求
func handleAuth(w http.ResponseWriter, r *http.Request) {
	log.Printf("[AuthHandler] 收到令牌请求: %s", r.URL.String())

	query := r.URL.Query()
	originalRealm := query.Get("original_realm")
	service := query.Get("service")
	scope := query.Get("scope")

	log.Printf("[AuthHandler] 解析参数 - original_realm: %s, service: %s, scope: %s", originalRealm, service, scope)

	if originalRealm == "" {
		log.Printf("[AuthHandler] 错误: 缺少 original_realm 参数")
		http.Error(w, "缺少 original_realm 参数", http.StatusBadRequest)
		return
	}

	upstreamAuthURL, err := url.Parse(originalRealm)
	if err != nil {
		log.Printf("[AuthHandler] 错误: 无效的 original_realm: %v", err)
		http.Error(w, "无效的 original_realm", http.StatusInternalServerError)
		return
	}
	
	// 构建查询参数
	authQuery := upstreamAuthURL.Query()
	if service != "" {
		authQuery.Set("service", service)
	}
	if scope != "" {
		authQuery.Set("scope", scope)
	}
	
	for key, values := range r.URL.Query() {
		if key != "original_realm" && key != "service" && key != "scope" {
			for _, value := range values {
				authQuery.Add(key, value)
			}
		}
	}
	
	upstreamAuthURL.RawQuery = authQuery.Encode()

	clientAuthHeader := r.Header.Get("Authorization")

	log.Printf("[AuthHandler] 中继认证到: %s", upstreamAuthURL.String())

	authReq, err := http.NewRequest("GET", upstreamAuthURL.String(), nil)
	if err != nil {
		log.Printf("[AuthHandler] 错误: 创建上游认证请求失败: %v", err)
		http.Error(w, "创建上游认证请求失败", http.StatusInternalServerError)
		return
	}
	
	if clientAuthHeader != "" {
		authReq.Header.Set("Authorization", clientAuthHeader)
	}
	
	for key, values := range r.Header {
		if key != "Authorization" && strings.HasPrefix(strings.ToLower(key), "x-") {
			authReq.Header[key] = values
		}
	}

	resp, err := sharedHTTPClient.Do(authReq)
	if err != nil {
		log.Printf("[AuthHandler] 错误: 请求上游认证失败: %v", err)
		http.Error(w, "请求上游认证失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	log.Printf("[AuthHandler] 上游认证响应状态: %s", resp.Status)

	// 返回响应
	for key, values := range resp.Header {
		w.Header()[key] = values
	}
	w.WriteHeader(resp.StatusCode)

	if _, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("[AuthHandler] 流式返回令牌时出错: %v", err)
	} else {
		log.Printf("[AuthHandler] 成功返回令牌给客户端")
	}
}

// 处理根路径请求
func handleRoot(w http.ResponseWriter, r *http.Request) {
	// 健康检查
	if r.URL.Path == "/" {
		log.Printf("[HealthCheck] 收到健康检查请求: %s %s", r.Method, r.URL.Path)
		
		// 获取当前域名（不含协议）
		currentDomain := r.Host
		if currentDomain == "" {
			currentDomain = r.Header.Get("Host")
		}
		
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		
		htmlContent := `<!DOCTYPE html>
<html lang="zh-CN">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Docker 代理推送组件</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body { 
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
            background: #f5f5f5;
            min-height: 100vh;
            display: flex;
            align-items: center;
            justify-content: center;
            color: #333;
        }
        .container {
            background: #ffffff;
            border-radius: 8px;
            padding: 40px;
            max-width: 600px;
            width: 90%;
            box-shadow: 0 2px 10px rgba(0,0,0,0.1);
            border: 1px solid #e0e0e0;
        }
        .status {
            display: flex;
            align-items: center;
            margin-bottom: 30px;
            gap: 15px;
        }
        .status-dot {
            width: 12px;
            height: 12px;
            background: #28a745;
            border-radius: 50%;
            animation: pulse 2s infinite;
        }
        @keyframes pulse {
            0% { transform: scale(1); opacity: 1; }
            50% { transform: scale(1.1); opacity: 0.7; }
            100% { transform: scale(1); opacity: 1; }
        }
        h1 { 
            color: #212529; 
            font-size: 24px; 
            font-weight: 600;
        }
        .description {
            background: #f8f9fa;
            padding: 20px;
            border-radius: 4px;
            margin: 20px 0;
            border-left: 4px solid #007bff;
        }
        .usage {
            background: #f8f9fa;
            color: #212529;
            padding: 20px;
            border-radius: 4px;
            font-family: 'Monaco', 'Menlo', monospace;
            font-size: 14px;
            line-height: 1.6;
            overflow-x: auto;
            border: 1px solid #dee2e6;
        }
        .usage h3 {
            color: #495057;
            margin-bottom: 15px;
            font-size: 16px;
        }
        .command {
            color: #212529;
            display: block;
            margin: 8px 0;
        }
        .comment {
            color: #6c757d;
            font-style: italic;
        }
        .highlight {
            background: #e9ecef;
            padding: 2px 6px;
            border-radius: 3px;
            color: #495057;
            font-weight: 600;
        }
        .footer {
            text-align: center;
            margin-top: 30px;
            color: #666;
            font-size: 14px;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="status">
            <div class="status-dot"></div>
            <h1>Docker 代理推送组件</h1>
        </div>
        
                 <div class="description">
             <p><strong>🔒 安全透明</strong>：本服务采用认证透传方案，不会存储和缓存您的敏感信息，请放心使用。</p>
         </div>

        <div class="usage">
            <h3>📋 客户端使用方法</h3>
            
            <span class="comment"># 1. 登录（使用 Docker Hub 登录凭据）</span>
            <span class="command">docker login ` + currentDomain + `</span>
            
                         <span class="comment"># 2. 标记镜像</span>
             <span class="command">docker tag alpine:latest ` + currentDomain + `/username/alpine:latest</span>
             
             <span class="comment"># 3. 推送镜像</span>
             <span class="command">docker push ` + currentDomain + `/username/alpine:latest</span>
             
             <span class="comment"># 4. 查看推送成功</span>
        </div>

        <div class="footer">
            <p>上游仓库：<strong>Docker Hub</strong></p>
            <p>服务状态：<span style="color: #28a745;">✓ 正常运行</span></p>
        </div>
    </div>
</body>
</html>`
		
		w.Write([]byte(htmlContent))
		return
	}
	
	// 处理CDN blob请求
	if strings.Contains(r.URL.Path, "registry-v2/docker/registry/v2/blobs/") {
		originalURL := "https://production.cloudflare.docker.com" + r.URL.Path
		if r.URL.RawQuery != "" {
			originalURL += "?" + r.URL.RawQuery
		}
		
		log.Printf("[CDNHandler] 处理CDN请求: %s", originalURL)
		cdnHandler := createCDNProxy(originalURL)
		cdnHandler(w, r)
		return
	}
	
	// 包装ResponseWriter以处理认证和Location重写
	proxyBaseURL := getProxyBaseURL(r)
	wrappedWriter := &responseRewriter{
		ResponseWriter: w,
		proxyBaseURL:   proxyBaseURL,
	}
	
	// 使用代理处理其他请求
	log.Printf("[Proxy] 转发请求: %s %s", r.Method, r.URL.Path)
	streamingProxy.ServeHTTP(wrappedWriter, r)
}

func main() {
	// 读取配置
	cfg := GetConfig()
	
	// 初始化HTTP客户端（使用配置的超时时间）
	sharedHTTPClient = &http.Client{
		Timeout: time.Duration(cfg.Timeouts.ClientTimeout) * time.Second,
	}
	sharedCDNClient = &http.Client{
		Timeout: time.Duration(cfg.Timeouts.CDNTimeout) * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	
	// 构建监听地址
	listenAddr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	
	// 初始化代理
	streamingProxy = createStreamingProxy()
	
	// 初始化全局限流器
	globalLimiter = initGlobalLimiter(cfg)
	
	mux := http.NewServeMux()

	mux.HandleFunc("/auth", handleAuth)
	mux.HandleFunc("/", handleRoot)
	
	// 应用限流中间件
	handler := rateLimitMiddleware(mux)

	log.Printf("Docker 仓库认证代理启动，监听在 %s", listenAddr)
	log.Printf("上游仓库设置为: %s", UPSTREAM_REGISTRY_HOST)

	server := &http.Server{
		Addr:    listenAddr,
		Handler: handler,
		ReadTimeout:       time.Duration(cfg.Timeouts.ReadTimeout) * time.Second,
		WriteTimeout:      time.Duration(cfg.Timeouts.WriteTimeout) * time.Second,
		ReadHeaderTimeout: time.Duration(cfg.Timeouts.ReadHeaderTimeout) * time.Second,
		IdleTimeout:       time.Duration(cfg.Timeouts.IdleTimeout) * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("代理服务器启动失败: %v", err)
	}
} 
