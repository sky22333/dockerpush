// main.go
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/vulcand/oxy/v2/forward"
)

// 配置常量
const (
	UPSTREAM_REGISTRY_HOST = "registry-1.docker.io"
	PROXY_LISTEN_ADDR = ":5002"
)

// 正则表达式
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

// 代理处理器
var streamingProxy http.Handler

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
		
		client := &http.Client{
			Timeout: 60 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		
		resp, err := client.Do(cdnReq)
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

	client := &http.Client{
		Timeout: 30 * time.Second,
	}
	resp, err := client.Do(authReq)
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok","service":"docker-registry-proxy","upstream":"` + UPSTREAM_REGISTRY_HOST + `"}`))
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
	// 初始化代理
	streamingProxy = createStreamingProxy()
	
	mux := http.NewServeMux()

	mux.HandleFunc("/auth", handleAuth)
	mux.HandleFunc("/", handleRoot)

	log.Printf("Docker 仓库认证代理启动，监听在 %s", PROXY_LISTEN_ADDR)
	log.Printf("上游仓库设置为: %s", UPSTREAM_REGISTRY_HOST)

	server := &http.Server{
		Addr:    PROXY_LISTEN_ADDR,
		Handler: mux,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("代理服务器启动失败: %v", err)
	}
} 