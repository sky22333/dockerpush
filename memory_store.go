package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// MemoryStore 内存存储实现 - 简化版，仅支持透传模式
type MemoryStore struct {
	tokens         map[string]*TokenInfo
	uploadSessions map[string]*UploadSession
	mu            sync.RWMutex
	logger        *zap.Logger
	
	// 清理器
	cleanupTicker *time.Ticker
	stopCleanup   chan struct{}
}

// TokenInfo 定义在 memory_auth_service.go 中



// NewMemoryStore 创建内存存储
func NewMemoryStore() *MemoryStore {
	logger, _ := zap.NewProduction()
	
	store := &MemoryStore{
		tokens:         make(map[string]*TokenInfo),
		uploadSessions: make(map[string]*UploadSession),
		logger:         logger,
		stopCleanup:    make(chan struct{}),
	}
	
	// 启动定期清理
	store.startCleanup()
	
	return store
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



// 获取统计信息
func (ms *MemoryStore) GetStats() map[string]interface{} {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	
	return map[string]interface{}{
		"active_tokens":        len(ms.tokens),
		"active_upload_sessions": len(ms.uploadSessions),
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
	
	// 清理超时的上传会话（1小时无活动）
	for uuid, session := range ms.uploadSessions {
		if now.Sub(session.StartTime) > 1*time.Hour {
			delete(ms.uploadSessions, uuid)
		}
	}
	
	ms.logger.Debug("Cleanup completed",
		zap.Int("active_tokens", len(ms.tokens)),
		zap.Int("active_sessions", len(ms.uploadSessions)))
}

// 停止清理器
func (ms *MemoryStore) Stop() {
	close(ms.stopCleanup)
}

// 数据持久化（可选） - 简化版
func (ms *MemoryStore) ExportData() ([]byte, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	
	data := map[string]interface{}{
		"export_time": time.Now(),
		"note":        "passthrough mode - no user data stored",
	}
	
	return json.Marshal(data)
}

func (ms *MemoryStore) ImportData(data []byte) error {
	// 透传模式下无需导入用户数据
	return nil
}



 
