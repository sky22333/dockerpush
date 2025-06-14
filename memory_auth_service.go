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

type TokenInfo struct {
	Username    string    `json:"username"`
	Roles       []string  `json:"roles"`
	Service     string    `json:"service"`
	Scope       string    `json:"scope"`
	Permissions []string  `json:"permissions"`
	IssuedAt    time.Time `json:"issued_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func (as *MemoryAuthService) GenerateToken(username, password, service, scope string) (string, error) {
	permissions := as.parsePermissionsForPassthrough(scope)
	clientAuth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))

	claims := TokenClaims{
		Username:    username,
		Roles:       []string{"user"},
		Service:     service,
		Scope:       scope,
		Permissions: permissions,
		ClientAuth:  clientAuth,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(as.tokenExpiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
			Issuer:    "registry-proxy",
			Subject:   username,
			ID:        as.generateJTI(),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(as.jwtSigningKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign token: %w", err)
	}

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

func (as *MemoryAuthService) ValidateToken(tokenString string) bool {
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

	_, err = as.store.GetToken(claims.ID)
	if err != nil {
		as.logger.Warn("Token not found in cache or expired",
			zap.String("jti", claims.ID))
		return false
	}

	as.logger.Debug("Token validated for passthrough user",
		zap.String("username", claims.Username))

	return true
}

func (as *MemoryAuthService) ParseToken(tokenString string) (*TokenClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &TokenClaims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return as.jwtSigningKey, nil
	})

	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(*TokenClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}

	return claims, nil
}

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

	as.store.DeleteToken(claims.ID)

	as.logger.Info("Token revoked",
		zap.String("username", claims.Username),
		zap.String("jti", claims.ID))

	return nil
}

func (as *MemoryAuthService) parsePermissionsForPassthrough(scope string) []string {
	var permissions []string
	
	if scope == "" {
		return permissions
	}

	parts := strings.Split(scope, ":")
	if len(parts) != 3 {
		return permissions
	}

	resourceType := parts[0]
	repository := parts[1]
	actions := strings.Split(parts[2], ",")

	for _, action := range actions {
		permissions = append(permissions, fmt.Sprintf("%s:%s:%s", resourceType, repository, action))
	}

	return permissions
}

func (as *MemoryAuthService) generateJTI() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.URLEncoding.EncodeToString(b)
}

func (as *MemoryAuthService) GetActiveTokenCount() int {
	stats := as.store.GetStats()
	if count, ok := stats["active_tokens"].(int); ok {
		return count
	}
	return 0
}

func (as *MemoryAuthService) CleanupExpiredTokens() {
	as.logger.Debug("Token cleanup triggered")
}

type TokenClaims struct {
	Username    string   `json:"username"`
	Roles       []string `json:"roles"`
	Service     string   `json:"service"`
	Scope       string   `json:"scope"`
	Permissions []string `json:"permissions"`
	ClientAuth  string   `json:"client_auth"`
	jwt.RegisteredClaims
} 