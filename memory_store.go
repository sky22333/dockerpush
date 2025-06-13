package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

// MemoryStore 内存存储实现
type MemoryStore struct {
	users         map[string]*User
	tokens        map[string]*TokenInfo
	uploadSessions map[string]*UploadSession
	authFailures  map[string]*AuthFailure
	mu            sync.RWMutex
	logger        *zap.Logger
	
	// 清理器
	cleanupTicker *time.Ticker
	stopCleanup   chan struct{}
}

// TokenInfo 定义在 memory_auth_service.go 中

// AuthFailure 认证失败记录
type AuthFailure struct {
	Count     int       `json:"count"`
	LastTry   time.Time `json:"last_try"`
	LockedUntil time.Time `json:"locked_until,omitempty"`
}

// NewMemoryStore 创建内存存储
func NewMemoryStore() *MemoryStore {
	logger, _ := zap.NewProduction()
	
	store := &MemoryStore{
		users:          make(map[string]*User),
		tokens:         make(map[string]*TokenInfo),
		uploadSessions: make(map[string]*UploadSession),
		authFailures:   make(map[string]*AuthFailure),
		logger:         logger,
		stopCleanup:    make(chan struct{}),
	}
	
	// 启动定期清理
	store.startCleanup()
	
	return store
}

// 用户管理实现
func (ms *MemoryStore) ValidateUser(username, password string) (*User, error) {
	ms.mu.RLock()
	user, exists := ms.users[username]
	ms.mu.RUnlock()
	
	if !exists {
		return nil, fmt.Errorf("user not found")
	}
	
	// 验证密码
	err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password))
	if err != nil {
		return nil, fmt.Errorf("invalid password")
	}
	
	return user, nil
}

func (ms *MemoryStore) GetUserByUsername(username string) (*User, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	
	user, exists := ms.users[username]
	if !exists {
		return nil, fmt.Errorf("user not found")
	}
	
	return user, nil
}

func (ms *MemoryStore) CreateUser(user *User) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	
	// 检查用户是否已存在
	if _, exists := ms.users[user.Username]; exists {
		return fmt.Errorf("user already exists")
	}
	
	// 哈希密码
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(user.PasswordHash), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}
	user.PasswordHash = string(hashedPassword)
	
	user.CreatedAt = time.Now()
	user.UpdatedAt = time.Now()
	
	ms.users[user.Username] = user
	
	ms.logger.Info("User created", zap.String("username", user.Username))
	return nil
}

func (ms *MemoryStore) UpdateUser(user *User) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	
	user.UpdatedAt = time.Now()
	ms.users[user.Username] = user
	
	return nil
}

func (ms *MemoryStore) DeleteUser(username string) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	
	delete(ms.users, username)
	ms.logger.Info("User deleted", zap.String("username", username))
	return nil
}

// 令牌管理
func (ms *MemoryStore) StoreToken(tokenID string, tokenInfo *TokenInfo) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	
	ms.tokens[tokenID] = tokenInfo
	return nil
}

func (ms *MemoryStore) GetToken(tokenID string) (*TokenInfo, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	
	tokenInfo, exists := ms.tokens[tokenID]
	if !exists {
		return nil, fmt.Errorf("token not found")
	}
	
	// 检查是否过期
	if time.Now().After(tokenInfo.ExpiresAt) {
		return nil, fmt.Errorf("token expired")
	}
	
	return tokenInfo, nil
}

func (ms *MemoryStore) DeleteToken(tokenID string) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	
	delete(ms.tokens, tokenID)
	return nil
}

// 上传会话管理
func (ms *MemoryStore) SetUploadSession(uuid string, session *UploadSession) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	
	ms.uploadSessions[uuid] = session
	return nil
}

func (ms *MemoryStore) GetUploadSession(uuid string) (*UploadSession, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	
	session, exists := ms.uploadSessions[uuid]
	if !exists {
		return nil, fmt.Errorf("upload session not found")
	}
	
	return session, nil
}

func (ms *MemoryStore) DeleteUploadSession(uuid string) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	
	delete(ms.uploadSessions, uuid)
	return nil
}

// 认证失败管理
func (ms *MemoryStore) RecordAuthFailure(username string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	
	failure, exists := ms.authFailures[username]
	if !exists {
		failure = &AuthFailure{
			Count:   0,
			LastTry: time.Now(),
		}
	}
	
	failure.Count++
	failure.LastTry = time.Now()
	
	// 如果失败次数超过5次，锁定15分钟
	if failure.Count >= 5 {
		failure.LockedUntil = time.Now().Add(15 * time.Minute)
	}
	
	ms.authFailures[username] = failure
}

func (ms *MemoryStore) IsAccountLocked(username string) bool {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	
	failure, exists := ms.authFailures[username]
	if !exists {
		return false
	}
	
	return time.Now().Before(failure.LockedUntil)
}

func (ms *MemoryStore) ClearAuthFailures(username string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	
	delete(ms.authFailures, username)
}

// 获取统计信息
func (ms *MemoryStore) GetStats() map[string]interface{} {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	
	return map[string]interface{}{
		"total_users":          len(ms.users),
		"active_tokens":        len(ms.tokens),
		"active_upload_sessions": len(ms.uploadSessions),
		"failed_auth_attempts": len(ms.authFailures),
	}
}

// 定期清理过期数据
func (ms *MemoryStore) startCleanup() {
	ms.cleanupTicker = time.NewTicker(5 * time.Minute)
	
	go func() {
		for {
			select {
			case <-ms.cleanupTicker.C:
				ms.cleanup()
			case <-ms.stopCleanup:
				ms.cleanupTicker.Stop()
				return
			}
		}
	}()
}

func (ms *MemoryStore) cleanup() {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	
	now := time.Now()
	
	// 清理过期token
	for tokenID, tokenInfo := range ms.tokens {
		if now.After(tokenInfo.ExpiresAt) {
			delete(ms.tokens, tokenID)
		}
	}
	
	// 清理过期的认证失败记录
	for username, failure := range ms.authFailures {
		if now.After(failure.LockedUntil) && failure.Count < 5 {
			delete(ms.authFailures, username)
		}
	}
	
	// 清理超时的上传会话（1小时无活动）
	for uuid, session := range ms.uploadSessions {
		if now.Sub(session.StartTime) > 1*time.Hour {
			delete(ms.uploadSessions, uuid)
		}
	}
	
	ms.logger.Debug("Cleanup completed",
		zap.Int("active_tokens", len(ms.tokens)),
		zap.Int("active_sessions", len(ms.uploadSessions)),
		zap.Int("auth_failures", len(ms.authFailures)))
}

// 停止清理器
func (ms *MemoryStore) Stop() {
	close(ms.stopCleanup)
}

// 数据持久化（可选）
func (ms *MemoryStore) ExportData() ([]byte, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	
	data := map[string]interface{}{
		"users":         ms.users,
		"export_time":   time.Now(),
	}
	
	return json.Marshal(data)
}

func (ms *MemoryStore) ImportData(data []byte) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	
	var importData map[string]interface{}
	if err := json.Unmarshal(data, &importData); err != nil {
		return fmt.Errorf("failed to unmarshal import data: %w", err)
	}
	
	// 导入用户数据
	if usersData, exists := importData["users"]; exists {
		usersJSON, _ := json.Marshal(usersData)
		var users map[string]*User
		if err := json.Unmarshal(usersJSON, &users); err == nil {
			ms.users = users
		}
	}
	
	return nil
}

// 高性能读写锁优化的批量操作
func (ms *MemoryStore) BatchGetUsers(usernames []string) map[string]*User {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	
	result := make(map[string]*User)
	for _, username := range usernames {
		if user, exists := ms.users[username]; exists {
			result[username] = user
		}
	}
	
	return result
}

func (ms *MemoryStore) BatchDeleteTokens(tokenIDs []string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	
	for _, tokenID := range tokenIDs {
		delete(ms.tokens, tokenID)
	}
}

// 内存使用优化
func (ms *MemoryStore) OptimizeMemory() {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	
	// 强制垃圾回收
	ms.cleanup()
	
	// 重建map以减少内存碎片
	newUsers := make(map[string]*User, len(ms.users))
	for k, v := range ms.users {
		newUsers[k] = v
	}
	ms.users = newUsers
	
	newTokens := make(map[string]*TokenInfo, len(ms.tokens))
	for k, v := range ms.tokens {
		newTokens[k] = v
	}
	ms.tokens = newTokens
} 