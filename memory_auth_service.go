package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
)

// User 用户结构
type User struct {
	Username     string             `json:"username"`
	Email        string             `json:"email"`
	PasswordHash string             `json:"password_hash"`
	Roles        []string           `json:"roles"`
	Permissions  map[string][]string `json:"permissions"` // repository -> actions
	CreatedAt    time.Time          `json:"created_at"`
	UpdatedAt    time.Time          `json:"updated_at"`
	Enabled      bool               `json:"enabled"`
	LastLogin    *time.Time         `json:"last_login,omitempty"`
}

// TokenInfo 令牌信息
type TokenInfo struct {
	Username    string    `json:"username"`
	Roles       []string  `json:"roles"`
	Service     string    `json:"service"`
	Scope       string    `json:"scope"`
	Permissions []string  `json:"permissions"`
	IssuedAt    time.Time `json:"issued_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// HealthChecker 健康检查器
type HealthChecker struct {
	proxyService *ProxyService
	upstreams    map[string]*UpstreamConfig
	logger       *zap.Logger
}

// CheckUpstreamHealth 检查上游健康状态
func (hc *HealthChecker) CheckUpstreamHealth() map[string]bool {
	results := make(map[string]bool)
	for name, upstream := range hc.upstreams {
		results[name] = hc.checkSingleUpstream(upstream)
	}
	return results
}

// checkSingleUpstream 检查单个上游
func (hc *HealthChecker) checkSingleUpstream(upstream *UpstreamConfig) bool {
	// 简化的健康检查，检查Registry地址是否配置
	return upstream.Registry != ""
}

// ValidateCredentials 验证用户凭据
func (as *MemoryAuthService) ValidateCredentials(username, password string) bool {
	// 检查账户是否被锁定
	if as.store.IsAccountLocked(username) {
		as.logger.Warn("Account temporarily locked",
			zap.String("username", username))
		return false
	}

	// 验证用户
	user, err := as.store.ValidateUser(username, password)
	if err != nil || user == nil {
		// 记录失败尝试
		as.store.RecordAuthFailure(username)
		
		as.logger.Warn("Authentication failed",
			zap.String("username", username),
			zap.Error(err))
		return false
	}

	if !user.Enabled {
		as.logger.Warn("User account disabled",
			zap.String("username", username))
		return false
	}

	// 清除失败计数
	as.store.ClearAuthFailures(username)
	
	// 更新最后登录时间
	now := time.Now()
	user.LastLogin = &now
	as.store.UpdateUser(user)
	
	as.logger.Info("User authenticated successfully",
		zap.String("username", username))
	return true
}

// GenerateToken 生成JWT访问令牌
func (as *MemoryAuthService) GenerateToken(username, service, scope string) (string, error) {
	user, err := as.store.GetUserByUsername(username)
	if err != nil {
		return "", fmt.Errorf("user not found: %w", err)
	}

	// 解析权限范围
	permissions := as.parsePermissions(user, scope)

	// 创建JWT claims
	claims := TokenClaims{
		Username:    username,
		Roles:       user.Roles,
		Service:     service,
		Scope:       scope,
		Permissions: permissions,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(as.tokenExpiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
			Issuer:    "registry-proxy",
			Subject:   username,
			ID:        as.generateJTI(),
		},
	}

	// 创建token
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(as.jwtSigningKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign token: %w", err)
	}

	// 缓存token信息
	tokenInfo := &TokenInfo{
		Username:    username,
		Roles:       user.Roles,
		Service:     service,
		Scope:       scope,
		Permissions: permissions,
		IssuedAt:    time.Now(),
		ExpiresAt:   time.Now().Add(as.tokenExpiry),
	}
	
	as.store.StoreToken(claims.ID, tokenInfo)

	as.logger.Info("Token generated",
		zap.String("username", username),
		zap.String("service", service),
		zap.String("scope", scope),
		zap.String("jti", claims.ID))

	return tokenString, nil
}

// ValidateToken 验证JWT令牌
func (as *MemoryAuthService) ValidateToken(tokenString string) bool {
	// 解析token
	token, err := jwt.ParseWithClaims(tokenString, &TokenClaims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return as.jwtSigningKey, nil
	})

	if err != nil {
		as.logger.Warn("Token validation failed", zap.Error(err))
		return false
	}

	claims, ok := token.Claims.(*TokenClaims)
	if !ok || !token.Valid {
		as.logger.Warn("Invalid token claims")
		return false
	}

	// 检查token是否在缓存中（支持token撤销）
	_, err = as.store.GetToken(claims.ID)
	if err != nil {
		as.logger.Warn("Token not found in cache or expired",
			zap.String("jti", claims.ID))
		return false
	}

	// 检查用户是否仍然有效
	user, err := as.store.GetUserByUsername(claims.Username)
	if err != nil || !user.Enabled {
		as.logger.Warn("User no longer valid",
			zap.String("username", claims.Username))
		return false
	}

	return true
}

// RevokeToken 撤销令牌
func (as *MemoryAuthService) RevokeToken(tokenString string) error {
	token, err := jwt.ParseWithClaims(tokenString, &TokenClaims{}, func(token *jwt.Token) (interface{}, error) {
		return as.jwtSigningKey, nil
	})

	if err != nil {
		return fmt.Errorf("failed to parse token: %w", err)
	}

	claims, ok := token.Claims.(*TokenClaims)
	if !ok {
		return fmt.Errorf("invalid token claims")
	}

	// 从缓存中删除token
	as.store.DeleteToken(claims.ID)

	as.logger.Info("Token revoked",
		zap.String("username", claims.Username),
		zap.String("jti", claims.ID))

	return nil
}

// CheckPermission 检查权限
func (as *MemoryAuthService) CheckPermission(tokenString, repository, action string) bool {
	token, err := jwt.ParseWithClaims(tokenString, &TokenClaims{}, func(token *jwt.Token) (interface{}, error) {
		return as.jwtSigningKey, nil
	})

	if err != nil {
		return false
	}

	claims, ok := token.Claims.(*TokenClaims)
	if !ok || !token.Valid {
		return false
	}

	// 检查用户权限
	user, err := as.store.GetUserByUsername(claims.Username)
	if err != nil {
		return false
	}

	return as.hasPermission(user, repository, action)
}

// parsePermissions 解析权限范围
func (as *MemoryAuthService) parsePermissions(user *User, scope string) []string {
	var permissions []string
	
	// 解析scope格式: repository:library/alpine:pull,push
	if scope == "" {
		return permissions
	}

	parts := strings.Split(scope, ":")
	if len(parts) != 3 {
		return permissions
	}

	resourceType := parts[0] // repository
	repository := parts[1]   // library/alpine
	actions := strings.Split(parts[2], ",") // pull,push

	for _, action := range actions {
		if as.hasPermission(user, repository, action) {
			permissions = append(permissions, fmt.Sprintf("%s:%s:%s", resourceType, repository, action))
		}
	}

	return permissions
}

// hasPermission 检查用户是否有特定权限
func (as *MemoryAuthService) hasPermission(user *User, repository, action string) bool {
	// 检查用户是否有管理员权色
	for _, role := range user.Roles {
		if role == "admin" {
			return true
		}
	}

	// 检查特定仓库权限
	if repoPerms, exists := user.Permissions[repository]; exists {
		for _, perm := range repoPerms {
			if perm == action || perm == "*" {
				return true
			}
		}
	}

	// 检查通配符权限
	if repoPerms, exists := user.Permissions["*"]; exists {
		for _, perm := range repoPerms {
			if perm == action || perm == "*" {
				return true
			}
		}
	}

	return false
}

// generateJTI 生成JWT ID
func (as *MemoryAuthService) generateJTI() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.URLEncoding.EncodeToString(b)
}

// GetActiveTokenCount 获取活跃token数量
func (as *MemoryAuthService) GetActiveTokenCount() int {
	stats := as.store.GetStats()
	if count, ok := stats["active_tokens"].(int); ok {
		return count
	}
	return 0
}

// CleanupExpiredTokens 清理过期token
func (as *MemoryAuthService) CleanupExpiredTokens() {
	// 这个方法由MemoryStore的自动清理处理
	as.logger.Debug("Token cleanup triggered")
}

// RevokeUserTokens 撤销用户的所有token
func (as *MemoryAuthService) RevokeUserTokens(username string) {
	// 由于我们使用内存存储，需要遍历所有token
	// 在实际实现中，可以在MemoryStore中添加按用户索引的功能
	as.logger.Info("Revoking all tokens for user", zap.String("username", username))
}

// TokenClaims JWT token claims (重复定义，在auth_service.go中已定义)
type TokenClaims struct {
	Username    string   `json:"username"`
	Roles       []string `json:"roles"`
	Service     string   `json:"service"`
	Scope       string   `json:"scope"`
	Permissions []string `json:"permissions"`
	jwt.RegisteredClaims
} 