package server

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/fulfillment"
	"icloud-hme/internal/store"
)

type pickupAttempt struct {
	failures int
	resetAt  time.Time
}

type pickupRateLimiter struct {
	mu      sync.Mutex
	entries map[string]pickupAttempt
	max     int
	window  time.Duration
}

func newPickupRateLimiter(max int, window time.Duration) *pickupRateLimiter {
	return &pickupRateLimiter{entries: make(map[string]pickupAttempt), max: max, window: window}
}

func (l *pickupRateLimiter) allowed(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	attempt, ok := l.entries[key]
	if !ok || time.Now().After(attempt.resetAt) {
		delete(l.entries, key)
		return true
	}
	return attempt.failures < l.max
}

func (l *pickupRateLimiter) failure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	attempt, ok := l.entries[key]
	if !ok || now.After(attempt.resetAt) {
		attempt = pickupAttempt{resetAt: now.Add(l.window)}
	}
	attempt.failures++
	l.entries[key] = attempt
}

func (l *pickupRateLimiter) success(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

func (s *Server) publicSecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		c.Next()
	}
}

func (s *Server) requireFulfillment(c *gin.Context) bool {
	if s.fulfillment == nil {
		fail(c, http.StatusServiceUnavailable, "库存服务未配置，请设置 DATABASE_URL")
		return false
	}
	return true
}

type syncInventoryReq struct {
	AccountID string `json:"account_id" binding:"required"`
}

func (s *Server) syncInventory(c *gin.Context) {
	if !s.requireFulfillment(c) {
		return
	}
	var req syncInventoryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "account_id 必填")
		return
	}
	count, err := s.fulfillment.SyncAccount(c.Request.Context(), strings.TrimSpace(req.AccountID))
	if err != nil {
		fail(c, http.StatusBadGateway, "同步库存失败: "+err.Error())
		return
	}
	ok(c, gin.H{"account_id": req.AccountID, "synced": count})
}

func (s *Server) listInventory(c *gin.Context) {
	if !s.requireFulfillment(c) {
		return
	}
	limit, err := boundedQueryInt(c, "limit", 200, 1, 1000)
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	status := strings.TrimSpace(c.Query("status"))
	if status != "" && status != "available" && status != "reserved" && status != "disabled" && status != "retired" {
		fail(c, http.StatusBadRequest, "invalid status")
		return
	}
	items, err := s.fulfillment.ListMailboxes(c.Request.Context(), status, limit)
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	ok(c, gin.H{"count": len(items), "mailboxes": items})
}

type allocateOrderReq struct {
	ExternalID string `json:"external_id"`
	MailboxID  string `json:"mailbox_id"`
	TTLHours   int    `json:"ttl_hours"`
}

func (s *Server) allocateOrder(c *gin.Context) {
	if !s.requireFulfillment(c) {
		return
	}
	var req allocateOrderReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.TTLHours == 0 {
		req.TTLHours = 24
	}
	if req.TTLHours < 1 || req.TTLHours > 720 {
		fail(c, http.StatusBadRequest, "ttl_hours must be between 1 and 720")
		return
	}
	order, key, err := s.fulfillment.AllocateMailboxWithPickupKey(c.Request.Context(), req.MailboxID, req.ExternalID, time.Duration(req.TTLHours)*time.Hour)
	if errors.Is(err, store.ErrNoInventory) {
		fail(c, http.StatusConflict, "暂无可用邮箱库存")
		return
	}
	if err != nil {
		fail(c, http.StatusInternalServerError, "分配失败: "+err.Error())
		return
	}
	c.JSON(http.StatusCreated, apiResp{Success: true, Data: gin.H{
		"order": order, "email": order.MailboxAddress, "pickup_key": key, "pickup_code": key, "pickup_url": s.pickupURL(order.MailboxAddress, key), "delivery_text": s.deliveryText(order.MailboxAddress, key),
		"notice": "取件密钥和链接只在本次响应中返回，请立即交付或安全保存",
	}})
}

type allocateOrdersBatchReq struct {
	ExternalID string   `json:"external_id"`
	MailboxIDs []string `json:"mailbox_ids"`
	TTLHours   int      `json:"ttl_hours"`
}

func (s *Server) allocateOrdersBatch(c *gin.Context) {
	if !s.requireFulfillment(c) {
		return
	}
	var req allocateOrdersBatchReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.TTLHours == 0 {
		req.TTLHours = 24
	}
	if req.TTLHours < 1 || req.TTLHours > 720 {
		fail(c, http.StatusBadRequest, "ttl_hours must be between 1 and 720")
		return
	}
	if len(req.MailboxIDs) == 0 || len(req.MailboxIDs) > 750 {
		fail(c, http.StatusBadRequest, "mailbox_ids must contain between 1 and 750 items")
		return
	}

	seen := make(map[string]struct{}, len(req.MailboxIDs))
	deliveries := make([]gin.H, 0, len(req.MailboxIDs))
	failures := make([]gin.H, 0)
	externalPrefix := strings.TrimSpace(req.ExternalID)
	for index, rawMailboxID := range req.MailboxIDs {
		mailboxID := strings.TrimSpace(rawMailboxID)
		if mailboxID == "" {
			failures = append(failures, gin.H{"mailbox_id": rawMailboxID, "message": "邮箱编号为空"})
			continue
		}
		if _, duplicate := seen[mailboxID]; duplicate {
			continue
		}
		seen[mailboxID] = struct{}{}
		externalID := externalPrefix
		if externalID != "" && len(req.MailboxIDs) > 1 {
			externalID = fmt.Sprintf("%s-%03d", externalID, index+1)
		}
		order, key, err := s.fulfillment.AllocateMailboxWithPickupKey(c.Request.Context(), mailboxID, externalID, time.Duration(req.TTLHours)*time.Hour)
		if err != nil {
			message := "签发失败"
			if errors.Is(err, store.ErrNoInventory) {
				message = "邮箱不存在或已被签发"
			}
			failures = append(failures, gin.H{"mailbox_id": mailboxID, "message": message})
			continue
		}
		deliveries = append(deliveries, gin.H{
			"order": order, "email": order.MailboxAddress, "pickup_key": key,
			"pickup_url": s.pickupURL(order.MailboxAddress, key), "delivery_text": s.deliveryText(order.MailboxAddress, key),
		})
	}
	if len(deliveries) == 0 {
		fail(c, http.StatusConflict, "所选邮箱均无法签发")
		return
	}
	c.JSON(http.StatusCreated, apiResp{Success: true, Data: gin.H{
		"count": len(deliveries), "deliveries": deliveries, "failures": failures,
		"notice": "取件密钥和链接只在本次响应中返回，请立即复制全部发货信息",
	}})
}

func (s *Server) pickupURL(email, key string) string {
	fragment := url.Values{}
	fragment.Set("email", email)
	fragment.Set("key", key)
	pickupURL := "/pickup#" + fragment.Encode()
	if s.publicBaseURL != "" {
		pickupURL = s.publicBaseURL + pickupURL
	}
	return pickupURL
}

func (s *Server) deliveryText(email, key string) string {
	return strings.Join([]string{email, key, s.pickupURL(email, key)}, "---")
}

func (s *Server) listOrders(c *gin.Context) {
	if !s.requireFulfillment(c) {
		return
	}
	limit, err := boundedQueryInt(c, "limit", 200, 1, 1000)
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	items, err := s.fulfillment.ListOrders(c.Request.Context(), limit)
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	ok(c, gin.H{"count": len(items), "orders": items})
}

type reissueOrderReq struct {
	TTLHours int `json:"ttl_hours"`
}

func (s *Server) reissueOrderPickup(c *gin.Context) {
	if !s.requireFulfillment(c) {
		return
	}
	var req reissueOrderReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.TTLHours == 0 {
		req.TTLHours = 24
	}
	if req.TTLHours < 1 || req.TTLHours > 720 {
		fail(c, http.StatusBadRequest, "ttl_hours must be between 1 and 720")
		return
	}
	order, key, err := s.fulfillment.ReissuePickupKey(c.Request.Context(), c.Param("id"), time.Duration(req.TTLHours)*time.Hour)
	if err != nil {
		fail(c, http.StatusNotFound, "重新签发失败: "+err.Error())
		return
	}
	fragment := url.Values{}
	fragment.Set("email", order.MailboxAddress)
	fragment.Set("key", key)
	pickupURL := "/pickup#" + fragment.Encode()
	if s.publicBaseURL != "" {
		pickupURL = s.publicBaseURL + pickupURL
	}
	deliveryText := strings.Join([]string{order.MailboxAddress, key, pickupURL}, "---")
	ok(c, gin.H{"order": order, "email": order.MailboxAddress, "pickup_key": key, "pickup_url": pickupURL, "delivery_text": deliveryText})
}

func (s *Server) listOrderMessages(c *gin.Context) {
	if !s.requireFulfillment(c) {
		return
	}
	limit, err := boundedQueryInt(c, "limit", 100, 1, 200)
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	items, err := s.fulfillment.ListMessages(c.Request.Context(), c.Param("id"), limit)
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	ok(c, gin.H{"count": len(items), "messages": items})
}

func (s *Server) listUnifiedMessages(c *gin.Context) {
	if !s.requireFulfillment(c) {
		return
	}
	limit, err := boundedQueryInt(c, "limit", 100, 1, 500)
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	items, err := s.fulfillment.ListUnifiedMessages(
		c.Request.Context(), c.Query("mailbox_id"), c.Query("account_id"), c.Query("q"), limit,
	)
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	ok(c, gin.H{"count": len(items), "messages": items})
}

func (s *Server) postOfficeStats(c *gin.Context) {
	if !s.requireFulfillment(c) {
		return
	}
	stats, err := s.fulfillment.Stats(c.Request.Context())
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	ok(c, stats)
}

func (s *Server) collectMail(c *gin.Context) {
	if !s.requireFulfillment(c) {
		return
	}
	var req struct {
		AccountID string `json:"account_id"`
		ForwardTo string `json:"forward_to"`
	}
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			fail(c, http.StatusBadRequest, "请求格式错误")
			return
		}
	}
	count, failures, err := s.fulfillment.CollectFiltered(c.Request.Context(), strings.TrimSpace(req.AccountID), strings.TrimSpace(req.ForwardTo))
	if err != nil {
		fail(c, http.StatusBadGateway, "收件失败: "+err.Error())
		return
	}
	ok(c, gin.H{"processed": count, "failures": failures})
}

type publicPickupReq struct {
	Token string `json:"token"`
	Code  string `json:"code"`
	Key   string `json:"key"`
	Email string `json:"email"`
}

func (s *Server) publicPickup(c *gin.Context) {
	if !s.requireFulfillment(c) {
		return
	}
	var req publicPickupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "取件凭证格式错误")
		return
	}
	key := strings.TrimSpace(req.Key)
	legacy := key == ""
	if legacy && (strings.TrimSpace(req.Token) == "" || strings.TrimSpace(req.Code) == "") {
		fail(c, http.StatusBadRequest, "取件链接或密钥不能为空")
		return
	}
	if !legacy && !strings.HasPrefix(key, "tok_") {
		fail(c, http.StatusBadRequest, "取件密钥格式错误")
		return
	}
	limiterValue := key
	if legacy {
		limiterValue = strings.TrimSpace(req.Token)
	}
	attemptHash := sha256.Sum256([]byte(limiterValue))
	attemptKey := string(attemptHash[:])
	if !s.pickupLimiter.allowed(attemptKey) {
		fail(c, http.StatusTooManyRequests, "尝试次数过多，请稍后再试")
		return
	}
	var order *store.Order
	var messages []store.Message
	var err error
	if legacy {
		order, messages, err = s.fulfillment.Pickup(c.Request.Context(), req.Token, req.Code)
	} else {
		order, messages, err = s.fulfillment.PickupKey(c.Request.Context(), key)
	}
	if errors.Is(err, fulfillment.ErrInvalidPickup) {
		s.pickupLimiter.failure(attemptKey)
		fail(c, http.StatusUnauthorized, "取件链接或访问密钥无效")
		return
	}
	if err != nil {
		fail(c, http.StatusInternalServerError, "读取邮件失败")
		return
	}
	if email := strings.TrimSpace(req.Email); email != "" && !strings.EqualFold(email, order.MailboxAddress) {
		s.pickupLimiter.failure(attemptKey)
		fail(c, http.StatusUnauthorized, "取件邮箱与密钥不匹配")
		return
	}
	processed, refreshed, syncErr := s.fulfillment.CollectMailbox(c.Request.Context(), order.MailboxID)
	if syncErr == nil {
		if latest, listErr := s.fulfillment.ListMessages(c.Request.Context(), order.ID, 100); listErr == nil {
			messages = latest
		}
	}
	s.pickupLimiter.success(attemptKey)
	syncState := gin.H{"refreshed": refreshed, "processed": processed, "synced_at": time.Now()}
	if syncErr != nil {
		syncState["warning"] = "当前网页收件同步失败，已显示此前同步的邮件: " + syncErr.Error()
	}
	startsAt := order.CreatedAt
	if order.ActivatedAt != nil {
		startsAt = *order.ActivatedAt
	}
	ok(c, gin.H{
		"email": order.MailboxAddress, "mailbox_id": order.MailboxID,
		"recipient_scope": order.MailboxAddress,
		"starts_at":       startsAt, "expires_at": order.ExpiresAt,
		"messages": messages, "sync": syncState,
	})
}
