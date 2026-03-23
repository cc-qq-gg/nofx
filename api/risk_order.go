package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"nofx/trader"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

type nonceStore struct {
	mu     sync.Mutex
	values map[string]time.Time
}

func newNonceStore() *nonceStore {
	return &nonceStore{
		values: make(map[string]time.Time),
	}
}

func (s *nonceStore) Use(key string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for nonce, expiry := range s.values {
		if now.After(expiry) {
			delete(s.values, nonce)
		}
	}

	if expiry, exists := s.values[key]; exists && now.Before(expiry) {
		return fmt.Errorf("nonce重复")
	}

	s.values[key] = now.Add(ttl)
	return nil
}

func (s *Server) handleRiskOrder(c *gin.Context) {
	_, traderID, err := s.getTraderFromQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	at, err := s.traderManager.GetTrader(traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	raw, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取请求体失败"})
		return
	}

	apiKey := c.GetHeader("X-API-KEY")
	timestamp := c.GetHeader("X-TIMESTAMP")
	nonce := c.GetHeader("X-NONCE")
	signature := c.GetHeader("X-SIGNATURE")
	if apiKey == "" || timestamp == "" || nonce == "" || signature == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "缺少认证请求头"})
		return
	}

	if err := at.ValidateRiskRequest(c.Request.Method, c.FullPath(), apiKey, timestamp, nonce, signature, raw); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	if err := s.nonces.Use(traderID+":"+nonce, 5*time.Minute); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	var req trader.RiskOrderRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体JSON无效"})
		return
	}
	if req.Symbol == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "symbol不能为空"})
		return
	}

	resp, err := at.ExecuteRiskOrder(&req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, resp)
}
