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





// GenerateToken 生成JWT访问令牌 - 简化版，不依赖本地用户系统
func (as *MemoryAuthService) GenerateToken(username, service, scope string) (string, error) {
	// 🔥 透传模式：为通过上游验证的用户生成通用token
	// 解析权限范围（给予基本的推拉权限）
	permissions := as.parsePermissionsForPassthrough(scope)

	// 创建JWT claims
	claims := TokenClaims{
		Username:    username,
		Roles:       []string{"user"}, // 简化角色
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

	// 缓存token信息（简化版）
	tokenInfo := &TokenInfo{
		Username:    username,
		Roles:       []string{"user"},
		Service:     service,
		Scope:       scope,
		Permissions: permissions,
		IssuedAt:    time.Now(),
		ExpiresAt:   time.Now().Add(as.tokenExpiry),
	}
	
	as.store.StoreToken(claims.ID, tokenInfo)

	as.logger.Info("Passthrough token generated",
		zap.String("username", username),
		zap.String("service", service),
		zap.String("scope", scope),
		zap.String("jti", claims.ID))

	return tokenString, nil
}

// ValidateToken 验证JWT令牌 - 简化版，不依赖本地用户系统
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

	// 🔥 透传模式：只验证token的有效性，不检查本地用户状态
	as.logger.Debug("Token validated for passthrough user",
		zap.String("username", claims.Username))

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



// parsePermissionsForPassthrough 解析权限范围 - 透传模式，给予所有请求的权限
func (as *MemoryAuthService) parsePermissionsForPassthrough(scope string) []string {
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

	// 🔥 透传模式：授予所有请求的权限，由上游Registry决定最终权限
	for _, action := range actions {
		permissions = append(permissions, fmt.Sprintf("%s:%s:%s", resourceType, repository, action))
	}

	return permissions
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
	
	// 移除：客户端认证缓存清理 - 真正透传模式下不需要缓存清理
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