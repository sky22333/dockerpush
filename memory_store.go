package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

type MemoryStore struct {
	tokens         map[string]*TokenInfo
	uploadSessions map[string]*UploadSession
	mu            sync.RWMutex
	logger        *zap.Logger
	cleanupTicker *time.Ticker
	stopCleanup   chan struct{}
}

func NewMemoryStore() *MemoryStore {
	logger, _ := zap.NewProduction()
	
	store := &MemoryStore{
		tokens:         make(map[string]*TokenInfo),
		uploadSessions: make(map[string]*UploadSession),
		logger:         logger,
		stopCleanup:    make(chan struct{}),
	}
	
	store.startCleanup()
	
	return store
}
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

func (ms *MemoryStore) GetStats() map[string]interface{} {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	
	return map[string]interface{}{
		"active_tokens":        len(ms.tokens),
		"active_upload_sessions": len(ms.uploadSessions),
	}
}
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
	
	for tokenID, tokenInfo := range ms.tokens {
		if now.After(tokenInfo.ExpiresAt) {
			delete(ms.tokens, tokenID)
		}
	}
	
	for uuid, session := range ms.uploadSessions {
		if now.Sub(session.StartTime) > 1*time.Hour {
			delete(ms.uploadSessions, uuid)
		}
	}
	
	ms.logger.Debug("Cleanup completed",
		zap.Int("active_tokens", len(ms.tokens)),
		zap.Int("active_sessions", len(ms.uploadSessions)))
}

func (ms *MemoryStore) Stop() {
	close(ms.stopCleanup)
}

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
	return nil
}



 
