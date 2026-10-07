// Package server 提供 HTTP API,基于 Gin。
//
// 两个核心接口:
//
//	POST /api/create  — 在指定账号下创建一个 Hide My Email 别名
//	GET  /api/inbox   — 读取指定账号(或指定别名)收到的邮件
//
// 辅助接口(用于多账号管理):账号增删查、别名列表、设置 App 密码。
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/account"
	"icloud-hme/internal/fulfillment"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
)

type Server struct {
	mgr                  *account.Manager
	r                    *gin.Engine
	apiKey               string
	adminAuth            *adminAuthStore
	fulfillment          *fulfillment.Service
	publicBaseURL        string
	pickupLimiter        *pickupRateLimiter
	aliasCreateLimiter   *aliasCreateRateLimiter
	browserAuthMu        sync.Mutex
	aliasMutationMu      sync.Mutex
	browserAuthByToken   map[[32]byte]*browserAuthSession
	browserAuthByRequest map[string]*browserAuthSession
	sched                *Scheduler
	schedRootCtx         context.Context
	schedRootCancel      context.CancelFunc
	backgroundOnce       sync.Once
	idleWatcherOnce      sync.Once
	idleWatchersMu       sync.Mutex
	idleWatchers         map[string]context.CancelFunc
	// idleWatchRunFn 测试注入的账号 watcher 运行函数; nil 时使用真实实现。
	idleWatchRunFn func(ctx context.Context, s *Server, accountID string, acc *account.Account)
	dataDir        string
	maxBodyBytes   int64
	readTimeout    time.Duration
	writeTimeout   time.Duration
	idleTimeout    time.Duration
}

const (
	defaultMaxBodyBytes           = 1 << 20
	maxInboxLimit                 = 200
	maxInboxDays                  = 365
	maxAliasCreateIntervalSeconds = 86400
)

type Config struct {
	Debug         bool
	APIKey        string
	AdminAuthFile string
	Fulfillment   *fulfillment.Service
	PublicBaseURL string
	DataDir       string // 数据目录 (调度器任务持久化等)
	MaxBodyBytes  int64
	ReadTimeout   time.Duration
	WriteTimeout  time.Duration
	IdleTimeout   time.Duration
}

func New(mgr *account.Manager, debug bool) *Server { return NewWithConfig(mgr, Config{Debug: debug}) }

func NewWithConfig(mgr *account.Manager, cfg Config) *Server {
	s, err := NewWithConfigE(mgr, cfg)
	if err != nil {
		panic(err)
	}
	return s
}

func NewWithConfigE(mgr *account.Manager, cfg Config) (*Server, error) {
	if !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 15 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 60 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	adminAuth, err := openAdminAuthStore(cfg.AdminAuthFile)
	if err != nil {
		return nil, err
	}
	adminAuth.ensureBootstrapToken()
	s := &Server{mgr: mgr, apiKey: cfg.APIKey, adminAuth: adminAuth, fulfillment: cfg.Fulfillment, publicBaseURL: strings.TrimRight(cfg.PublicBaseURL, "/"), pickupLimiter: newPickupRateLimiter(5, 10*time.Minute), aliasCreateLimiter: newAliasCreateRateLimiter(), browserAuthByToken: make(map[[32]byte]*browserAuthSession), browserAuthByRequest: make(map[string]*browserAuthSession), dataDir: strings.TrimSpace(cfg.DataDir), maxBodyBytes: cfg.MaxBodyBytes, readTimeout: cfg.ReadTimeout, writeTimeout: cfg.WriteTimeout, idleTimeout: cfg.IdleTimeout}
	s.schedRootCtx, s.schedRootCancel = context.WithCancel(context.Background())
	s.r = gin.Default()
	_ = s.r.SetTrustedProxies(nil)
	s.register()
	return s, nil
}

func (s *Server) Run(addr string) error {
	s.startBackgroundLoops()
	return s.httpServer(addr).ListenAndServe()
}

func (s *Server) RunTLS(addr, certFile, keyFile string) error {
	s.startBackgroundLoops()
	return s.httpServer(addr).ListenAndServeTLS(certFile, keyFile)
}

// startBackgroundLoops 启动后台循环 (幂等): 新接口会话保活、IDLE 增量收件。
func (s *Server) startBackgroundLoops() {
	s.backgroundOnce.Do(func() {
		s.restoreSchedulerJobs()
		s.startAppleAccountKeepAlive(s.schedRootCtx)
		s.startIdleMailWatchers(s.schedRootCtx)
	})
}

// restoreSchedulerJobs 从数据目录恢复调度器任务（重启后继续运行）。
func (s *Server) restoreSchedulerJobs() {
	if s.dataDir == "" {
		return
	}
	if err := os.MkdirAll(s.dataDir, 0700); err != nil {
		log.Printf("创建数据目录失败, 调度器任务不持久化: %v", err)
		return
	}
	path := filepath.Join(s.dataDir, "scheduler-jobs.json")
	restored, err := s.ensureScheduler().RestoreAndStartPersisted(path, s.schedRootCtx)
	if err != nil {
		log.Printf("恢复调度器任务失败: %v", err)
		return
	}
	if restored > 0 {
		log.Printf("已恢复 %d 个定时创建任务", restored)
	}
}
func (s *Server) httpServer(addr string) *http.Server {
	return &http.Server{Addr: addr, Handler: s.r, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: s.readTimeout, WriteTimeout: s.writeTimeout, IdleTimeout: s.idleTimeout, MaxHeaderBytes: 1 << 20}
}
func (s *Server) Handler() http.Handler { return s.r }

func (s *Server) register() {
	s.r.GET("/healthz", func(c *gin.Context) { ok(c, gin.H{"service": "icloud-hme-api"}) })
	auth := s.r.Group("/api/auth")
	auth.Use(s.secureHeaders(), s.limitRequestBody())
	auth.GET("/status", s.adminAuthStatus)
	auth.POST("/setup", s.setupAdmin)
	auth.POST("/login", s.loginAdmin)
	auth.POST("/logout", s.logoutAdmin)
	public := s.r.Group("/public")
	public.Use(s.publicSecurityHeaders(), s.limitRequestBody())
	public.POST("/pickup", s.publicPickup)
	public.POST("/browser-auth/complete", s.completeBrowserAuth)
	public.POST("/forwarding/web-auth/complete", s.completeForwardWebAuth)
	api := s.r.Group("/api")
	api.Use(s.secureHeaders(), s.limitRequestBody(), s.authenticate())
	{
		api.GET("/accounts", s.listAccounts)
		api.POST("/accounts", s.addAccount)
		api.DELETE("/accounts/:id", s.removeAccount)
		api.POST("/accounts/:id/password", s.setAppPassword)
		api.GET("/accounts/:id/forwarding", s.getForwarding)
		api.PUT("/accounts/:id/forwarding/default", s.setDefaultForwarding)
		api.POST("/accounts/:id/forwarding/web-auth/start", s.startForwardWebAuth)
		api.GET("/accounts/:id/forwarding/web-auth/status", s.forwardWebAuthStatus)
		api.PUT("/accounts/:id/cookies", s.updateCookies)
		api.POST("/accounts/:id/login", s.loginAccount)
		api.PUT("/accounts/:id/apple-account", s.setAppleAccount)
		api.PUT("/accounts/:id/apple-account/login", s.loginAppleAccount)
		api.DELETE("/accounts/:id/apple-account", s.clearAppleAccount)
		api.POST("/accounts/:id/browser-auth/start", s.startBrowserAuth)
		api.GET("/accounts/:id/browser-auth/status", s.browserAuthStatus)
		s.registerSchedulerRoutes(api)
		s.registerMachineAPI(api)
		api.POST("/create", s.createAlias)
		api.POST("/aliases", s.createAlias)
		api.GET("/inbox", s.listInbox)
		api.GET("/aliases", s.listAliases)
		api.POST("/aliases/:id/deactivate", s.deactivateAlias)
		api.POST("/aliases/:id/reactivate", s.reactivateAlias)
		api.DELETE("/aliases/:id", s.deleteAlias)
		api.POST("/inventory/sync", s.syncInventory)
		api.GET("/inventory", s.listInventory)
		api.GET("/mailboxes", s.listInventory)
		api.GET("/messages", s.listUnifiedMessages)
		api.GET("/post-office/stats", s.postOfficeStats)
		api.POST("/orders/allocate", s.allocateOrder)
		api.POST("/orders/allocate-batch", s.allocateOrdersBatch)
		api.GET("/orders", s.listOrders)
		api.POST("/orders/:id/reissue", s.reissueOrderPickup)
		api.GET("/orders/:id/messages", s.listOrderMessages)
		api.POST("/mail/collect", s.collectMail)
		api.POST("/reload", s.reloadConfig)
	}
}

func (s *Server) secureHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
		c.Next()
	}
}
func (s *Server) limitRequestBody() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, s.maxBodyBytes)
		}
		c.Next()
	}
}
func (s *Server) authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if token, err := c.Cookie(adminSessionCookie); err == nil && s.adminAuth.validateSession(token) {
			c.Next()
			return
		}
		if s.apiKey != "" {
			provided := strings.TrimSpace(c.GetHeader("X-API-Key"))
			if auth := strings.TrimSpace(c.GetHeader("Authorization")); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
				provided = strings.TrimSpace(auth[len("Bearer "):])
			}
			expectedHash := sha256.Sum256([]byte(s.apiKey))
			providedHash := sha256.Sum256([]byte(provided))
			if subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) == 1 {
				c.Next()
				return
			}
		}
		c.Header("WWW-Authenticate", "Session")
		c.AbortWithStatusJSON(http.StatusUnauthorized, apiResp{Success: false, Message: "登录已失效，请重新登录"})
	}
}

// ---- 统一响应 ----

type apiResp struct {
	Success           bool        `json:"success"`
	Message           string      `json:"message,omitempty"`
	Code              string      `json:"code,omitempty"`
	RetryAfterSeconds int64       `json:"retry_after_seconds,omitempty"`
	Data              interface{} `json:"data,omitempty"`
}

func ok(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, apiResp{Success: true, Data: data})
}

func fail(c *gin.Context, code int, msg string) {
	c.JSON(code, apiResp{Success: false, Message: msg})
}

func failAliasRateLimited(c *gin.Context, retryAfter time.Duration, msg string) {
	if retryAfter <= 0 {
		retryAfter = time.Hour
	}
	seconds := int64((retryAfter + time.Second - 1) / time.Second)
	c.Header("Retry-After", strconv.FormatInt(seconds, 10))
	c.JSON(http.StatusTooManyRequests, apiResp{
		Success:           false,
		Message:           msg,
		Code:              "alias_rate_limited",
		RetryAfterSeconds: seconds,
	})
}

// ====================================================================
// 核心接口 1: 创建邮箱
//   POST /api/create
//   body: {"account_id": "acc_xxx", "label": "可选标签", "interval_seconds": 3}
//   返回: 新创建的 HME 邮箱地址
// ====================================================================

type createReq struct {
	AccountID       string `json:"account_id" binding:"required"`
	Label           string `json:"label"`
	IntervalSeconds int    `json:"interval_seconds"`
}

func (s *Server) createAlias(c *gin.Context) {
	var req createReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: account_id 必填 — "+err.Error())
		return
	}
	if req.IntervalSeconds < 0 || req.IntervalSeconds > maxAliasCreateIntervalSeconds {
		fail(c, http.StatusBadRequest, "参数错误: interval_seconds 必须在 0 到 86400 之间")
		return
	}

	client, err := s.mgr.HMEClient(req.AccountID, false)
	if err != nil {
		fail(c, http.StatusNotFound, err.Error())
		return
	}

	// Serialize mutations so two requests cannot select the same random label
	// in one local server instance.
	s.aliasMutationMu.Lock()
	defer s.aliasMutationMu.Unlock()
	minimumInterval := time.Duration(req.IntervalSeconds) * time.Second
	if retryAfter := s.aliasCreateLimiter.retryAfter(req.AccountID, time.Now(), minimumInterval); retryAfter > 0 {
		failAliasRateLimited(c, retryAfter, "尚未达到自定义创建间隔；请在建议时间后重试")
		return
	}

	existing, err := client.ListAliases()
	if err != nil {
		_ = s.mgr.SaveCookies(req.AccountID, client.Cookies)
		// 旧接口会话失效时, 若已启用新接口 (Apple Account 管理) 仍可继续创建:
		// 跳过旧接口别名列表, 直接走新接口 (创建请求会先刷新新接口会话)。
		if _, aaOK := s.mgr.AppleAccountClient(req.AccountID); aaOK == nil && isSessionError(err) {
			req.Label = strings.TrimSpace(req.Label)
			if req.Label == "" {
				req.Label = "Created " + time.Now().Format("2006-01-02 15:04")
			}
			result, iface, createErr := s.createAliasPreferAppleAccount(req.AccountID, client, req.Label)
			if createErr != nil {
				fail(c, http.StatusBadGateway, "创建邮箱失败: "+createErr.Error())
				return
			}
			s.aliasCreateLimiter.recordSuccess(req.AccountID, time.Now())
			ok(c, gin.H{
				"email":                   result.Email,
				"label":                   result.Label,
				"created_at":              result.CreatedAt,
				"account_id":              req.AccountID,
				"interface":               iface,
				"active_count":            -1, // 旧接口不可用, 未能读取当前数量
				"managed_capacity_target": hme.ManagedCapacityTarget,
			})
			return
		}
		if isSessionError(err) {
			fail(c, http.StatusUnauthorized, "iCloud 会话失效，请重新完成网页授权: "+err.Error())
		} else {
			fail(c, http.StatusBadGateway, "创建前读取隐藏邮箱失败: "+err.Error())
		}
		return
	}
	activeCount := 0
	for _, alias := range existing {
		if alias.Active {
			activeCount++
		}
	}
	if strings.TrimSpace(req.Label) == "" {
		req.Label, err = hme.NewRandomLabel(existing)
		if err != nil {
			fail(c, http.StatusInternalServerError, err.Error())
			return
		}
	}

	// 优先新接口 (Apple Account 管理, 约 20 个/小时), 限速/会话失效时回退旧接口
	// (iCloud Web, 约 5 个/小时)。两接口配额独立, 合计约 25 个/小时。
	result, iface, err := s.createAliasPreferAppleAccount(req.AccountID, client, req.Label)

	// 操作完成后,保存可能已刷新的 Cookie（validate 会轮换 token）
	_ = s.mgr.SaveCookies(req.AccountID, client.Cookies)

	if err != nil {
		// 区分会话失效(需重新登录)与临时失败
		msg := err.Error()
		if errors.Is(err, hme.ErrAliasRateLimited) {
			retryAfter := hme.AliasRetryAfter(err)
			if retryAfter <= 0 {
				retryAfter = time.Hour
			}
			s.aliasCreateLimiter.block(req.AccountID, time.Now(), retryAfter)
			failAliasRateLimited(c, retryAfter, "Apple 暂时限制了创建频率，尚未达到总容量；请在建议时间后重试")
		} else if errors.Is(err, hme.ErrAliasLimitReached) {
			c.JSON(http.StatusConflict, apiResp{Success: false, Message: "Apple 明确拒绝创建：当前账号的隐藏邮件地址总容量已满", Code: "alias_capacity_reached"})
		} else if isSessionError(err) {
			fail(c, http.StatusUnauthorized, "iCloud 会话失效,请更新 Cookie: "+msg)
		} else {
			fail(c, http.StatusBadGateway, "创建邮箱失败: "+msg)
		}
		return
	}
	s.aliasCreateLimiter.recordSuccess(req.AccountID, time.Now())

	ok(c, gin.H{
		"email":                   result.Email,
		"label":                   result.Label,
		"created_at":              result.CreatedAt,
		"account_id":              req.AccountID,
		"interface":               iface, // apple_account | icloud_web
		"active_count":            activeCount + 1,
		"managed_capacity_target": hme.ManagedCapacityTarget,
		"remaining_to_target":     max(0, hme.ManagedCapacityTarget-activeCount-1),
	})
}

// createAliasPreferAppleAccount 依次尝试新接口与旧接口创建隐藏邮箱。
//
// 新接口 (Apple Account 管理) 配额约 20 个/小时, 旧接口 (iCloud Web) 约 5 个/小时,
// 两者配额独立。仅在新接口确实不可用时回退: 限速 / 会话失效 / 一般性失败。
// 新接口明确提示总容量已满时不回退 (总容量对两个接口是同一账号级限额)。
func (s *Server) createAliasPreferAppleAccount(accountID string, client *hme.Client, label string) (*hme.CreateResult, string, error) {
	aaClient, aaErr := s.mgr.AppleAccountClient(accountID)
	if aaErr == nil {
		result, err := aaClient.CreateAlias(label)
		_ = s.mgr.SaveAppleAccountState(accountID, aaClient.State())
		if err == nil {
			return result, "apple_account", nil
		}
		if errors.Is(err, hme.ErrAliasLimitReached) {
			// 总容量已满, 回退旧接口没有意义
			return nil, "apple_account", err
		}
		// 限速 / 会话失效 / 一般失败 → 落到旧接口继续尝试
	}
	result, err := client.CreateAlias(label, 5)
	if err != nil {
		return nil, "icloud_web", err
	}
	return result, "icloud_web", nil
}

// ====================================================================
// 核心接口 2: 读取邮件
//   GET /api/inbox?account_id=acc_xxx[&alias=xxx@icloud.com][&limit=20][&days=7]
//
//   - 不传 alias: 返回该账号收件箱最近邮件
//   - 传 alias:   只返回发给该 HME 别名的邮件
//
//   认证优先级: IMAP (App Password) 优先 > Web API (Cookie) 回退
//   - IMAP: 支持服务端按收件人搜索 (FindByRecipient)
//   - Web API: 不支持收件人搜索,拉取收件箱后本地按别名过滤 (FindByAlias)
// ====================================================================

func (s *Server) listInbox(c *gin.Context) {
	accountID := c.Query("account_id")
	if accountID == "" {
		fail(c, http.StatusBadRequest, "参数缺失: account_id")
		return
	}
	alias := strings.TrimSpace(c.Query("alias"))
	limit, err := boundedQueryInt(c, "limit", 20, 1, maxInboxLimit)
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	days, err := boundedQueryInt(c, "days", 7, 0, maxInboxDays)
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}

	// 优先使用 IMAP (App Password 认证)
	mc, err := s.mgr.MailClient(accountID)
	if err == nil {
		if connErr := mc.Connect(); connErr == nil {
			defer mc.Disconnect()
			var messages []mail.Message
			if alias != "" {
				messages, err = mc.FindByRecipient(alias, limit, days)
			} else {
				messages, err = mc.ListInbox(limit, days)
			}
			if err == nil {
				ok(c, gin.H{
					"account_id": accountID,
					"alias":      alias,
					"count":      len(messages),
					"messages":   messages,
					"method":     "imap",
				})
				return
			}
			// IMAP 失败，继续尝试 Web API
		}
	}

	// 回退到 Web API (Cookie 认证，无需 App Password)
	wmc, err := s.mgr.WebMailClient(accountID)
	if err != nil {
		fail(c, http.StatusBadRequest, "无可用邮件客户端: 需要 App Password 或 Cookie")
		return
	}

	if alias != "" {
		messages, err := wmc.FindByAlias(alias, limit)
		if err != nil {
			fail(c, http.StatusBadGateway, "读取邮件失败: "+err.Error())
			return
		}
		ok(c, gin.H{
			"account_id": accountID,
			"alias":      alias,
			"count":      len(messages),
			"messages":   messages,
			"method":     "web_api",
		})
	} else {
		messages, err := wmc.ListInbox(limit)
		if err != nil {
			fail(c, http.StatusBadGateway, "读取邮件失败: "+err.Error())
			return
		}
		ok(c, gin.H{
			"account_id": accountID,
			"count":      len(messages),
			"messages":   messages,
			"method":     "web_api",
		})
	}
}

// ====================================================================
// 辅助接口
// ====================================================================

func (s *Server) listAccounts(c *gin.Context) {
	accounts := s.mgr.ListAccounts()
	type accountView struct {
		*account.Account
		AppleAccountStatus map[string]any `json:"apple_account_status"`
	}
	views := make([]accountView, 0, len(accounts))
	for _, acc := range accounts {
		views = append(views, accountView{Account: acc, AppleAccountStatus: acc.AppleAccountStatus()})
	}
	ok(c, views)
}

type addAccountReq struct {
	Name    string `json:"name" binding:"required"`
	Cookies string `json:"cookies"` // 可选,后续可通过 /login 获取
	Host    string `json:"host"`
	Proxy   string `json:"proxy"` // HTTP/SOCKS5 代理
}

func (s *Server) addAccount(c *gin.Context) {
	var req addAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: name 必填 — "+err.Error())
		return
	}
	acc, err := s.mgr.AddAccount(req.Name, req.Cookies, req.Host, req.Proxy)
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	// 返回时脱敏
	acc.Cookies = nil
	c.JSON(http.StatusCreated, apiResp{Success: true, Data: acc})
}

func (s *Server) removeAccount(c *gin.Context) {
	id := c.Param("id")
	if !s.mgr.RemoveAccount(id) {
		fail(c, http.StatusNotFound, "账号不存在")
		return
	}
	ok(c, gin.H{"id": id})
}

type setPwdReq struct {
	ICloudEmail string `json:"icloud_email" binding:"required"`
	AppPassword string `json:"app_password" binding:"required"`
}

func (s *Server) setAppPassword(c *gin.Context) {
	id := c.Param("id")
	var req setPwdReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: icloud_email, app_password 必填 — "+err.Error())
		return
	}
	if err := s.mgr.SetAppPassword(id, req.ICloudEmail, req.AppPassword); err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	ok(c, gin.H{"id": id, "icloud_email": req.ICloudEmail})
}

func (s *Server) getForwarding(c *gin.Context) {
	id := c.Param("id")
	if _, err := s.mgr.RefreshForwarding(id); err != nil {
		fail(c, http.StatusBadGateway, "读取 Apple 转发设置失败: "+err.Error())
		return
	}
	acc, okAccount := s.mgr.GetAccount(id)
	if !okAccount {
		fail(c, http.StatusNotFound, "账号不存在")
		return
	}
	ok(c, acc.Redacted())
}

type setDefaultForwardingReq struct {
	Email string `json:"email" binding:"required"`
}

func (s *Server) setDefaultForwarding(c *gin.Context) {
	var req setDefaultForwardingReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "email 必填")
		return
	}
	if _, err := s.mgr.UpdateDefaultForwardTo(c.Param("id"), req.Email); err != nil {
		fail(c, http.StatusBadGateway, "修改 Apple 默认转发邮箱失败: "+err.Error())
		return
	}
	acc, _ := s.mgr.GetAccount(c.Param("id"))
	ok(c, acc.Redacted())
}

type updateCookiesReq struct {
	Cookies map[string]string `json:"cookies" binding:"required"`
}

func (s *Server) updateCookies(c *gin.Context) {
	id := c.Param("id")
	var req updateCookiesReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: cookies 必填 — "+err.Error())
		return
	}
	if err := s.mgr.UpdateCookies(id, req.Cookies); err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	ok(c, gin.H{"id": id, "cookies_count": len(req.Cookies)})
}

type loginReq struct {
	Password string `json:"password" binding:"required"`
	OTPCode  string `json:"otp_code"` // 可选 2FA 验证码
}

// setAppleAccount 导入 Apple Account 管理接口 (新接口) 的浏览器 Cookie 并引导会话。
//
// 引导成功返回新接口会话状态; 引导失败返回错误且不保存 (旧接口不受影响)。
// cookies 支持 Header String 或 JSON 格式, 与添加账号的粘贴格式一致。
func (s *Server) setAppleAccount(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Cookies any `json:"cookies" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: cookies 必填 — "+err.Error())
		return
	}
	cookies, err := parseCookiesAny(req.Cookies)
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	client, err := s.mgr.SetAppleAccount(id, cookies)
	if err != nil {
		fail(c, http.StatusBadGateway, err.Error())
		return
	}
	state := client.State()
	ok(c, gin.H{
		"id":               id,
		"cookies_count":    len(cookies),
		"apple_account":    state.Redacted(),
		"needs_refresh_ts": state.ManageExpiresAt,
	})
}

// clearAppleAccount 清除账号的新接口会话, 只保留旧接口。
func (s *Server) clearAppleAccount(c *gin.Context) {
	id := c.Param("id")
	if err := s.mgr.ClearAppleAccount(id); err != nil {
		fail(c, http.StatusNotFound, err.Error())
		return
	}
	ok(c, gin.H{"id": id, "apple_account": "cleared"})
}

// parseCookiesAny 解析 cookies 字段, 支持 map[string]string (JSON) 或 string (Header String)。
func parseCookiesAny(raw any) (map[string]string, error) {
	switch value := raw.(type) {
	case map[string]any:
		out := make(map[string]string, len(value))
		for k, v := range value {
			if s, ok := v.(string); ok && s != "" {
				out[k] = s
			}
		}
		if len(out) == 0 {
			return nil, errors.New("cookies 为空")
		}
		return out, nil
	case map[string]string:
		if len(value) == 0 {
			return nil, errors.New("cookies 为空")
		}
		return value, nil
	case string:
		return account.ParseCookieInput(value)
	default:
		return nil, errors.New("cookies 必须是 JSON 对象或 Header String")
	}
}

func (s *Server) loginAccount(c *gin.Context) {
	id := c.Param("id")
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: password 必填 — "+err.Error())
		return
	}

	var otpProvider hme.OTPProvider
	if req.OTPCode != "" {
		otp := req.OTPCode
		otpProvider = func() (string, error) {
			return otp, nil
		}
	}

	client, err := s.mgr.HMEClientWithPassword(id, req.Password, otpProvider)
	if err != nil {
		if isSessionError(err) {
			fail(c, http.StatusUnauthorized, err.Error())
		} else {
			fail(c, http.StatusBadGateway, "登录失败: "+err.Error())
		}
		return
	}

	ok(c, gin.H{
		"id":            id,
		"cookies_count": len(client.Cookies),
	})
}

func boundedQueryInt(c *gin.Context, name string, defaultValue, minValue, maxValue int) (int, error) {
	raw := c.DefaultQuery(name, strconv.Itoa(defaultValue))
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New(name + " must be an integer")
	}
	if value < minValue || value > maxValue {
		return 0, errors.New(name + " must be between " + strconv.Itoa(minValue) + " and " + strconv.Itoa(maxValue))
	}
	return value, nil
}
func (s *Server) listAliases(c *gin.Context) {
	accountID := c.Query("account_id")
	if accountID == "" {
		fail(c, http.StatusBadRequest, "参数缺失: account_id")
		return
	}
	client, err := s.mgr.HMEClient(accountID, false)
	if err != nil {
		fail(c, http.StatusNotFound, err.Error())
		return
	}
	aliases, err := client.ListAliases()
	_ = s.mgr.SaveCookies(accountID, client.Cookies)
	if err != nil {
		if isSessionError(err) {
			fail(c, http.StatusUnauthorized, "iCloud 会话失效,请更新 Cookie: "+err.Error())
		} else {
			fail(c, http.StatusBadGateway, err.Error())
		}
		return
	}
	activeCount := 0
	for _, alias := range aliases {
		if alias.Active {
			activeCount++
		}
	}
	ok(c, gin.H{
		"account_id":              accountID,
		"count":                   len(aliases),
		"active_count":            activeCount,
		"managed_capacity_target": hme.ManagedCapacityTarget,
		"remaining_to_target":     max(0, hme.ManagedCapacityTarget-activeCount),
		"aliases":                 aliases,
	})
}

type aliasActionReq struct {
	AccountID string `json:"account_id" binding:"required"`
}

func (s *Server) deactivateAlias(c *gin.Context) {
	anonymousID := c.Param("id")
	var req aliasActionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: account_id 必填 — "+err.Error())
		return
	}

	client, err := s.mgr.HMEClient(req.AccountID, false)
	if err != nil {
		fail(c, http.StatusNotFound, err.Error())
		return
	}

	success, err := client.DeactivateHME(anonymousID)
	_ = s.mgr.SaveCookies(req.AccountID, client.Cookies)
	if err != nil {
		fail(c, http.StatusBadGateway, "停用失败: "+err.Error())
		return
	}
	ok(c, gin.H{"anonymous_id": anonymousID, "success": success})
}

func (s *Server) reactivateAlias(c *gin.Context) {
	anonymousID := c.Param("id")
	var req aliasActionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: account_id 必填 — "+err.Error())
		return
	}

	client, err := s.mgr.HMEClient(req.AccountID, false)
	if err != nil {
		fail(c, http.StatusNotFound, err.Error())
		return
	}

	success, err := client.ReactivateHME(anonymousID)
	_ = s.mgr.SaveCookies(req.AccountID, client.Cookies)
	if err != nil {
		fail(c, http.StatusBadGateway, "激活失败: "+err.Error())
		return
	}
	ok(c, gin.H{"anonymous_id": anonymousID, "success": success})
}

func (s *Server) deleteAlias(c *gin.Context) {
	anonymousID := c.Param("id")
	var req aliasActionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: account_id 必填 — "+err.Error())
		return
	}

	client, err := s.mgr.HMEClient(req.AccountID, false)
	if err != nil {
		fail(c, http.StatusNotFound, err.Error())
		return
	}

	if err := client.Delete(anonymousID); err != nil {
		_ = s.mgr.SaveCookies(req.AccountID, client.Cookies)
		fail(c, http.StatusBadGateway, "删除失败: "+err.Error())
		return
	}
	_ = s.mgr.SaveCookies(req.AccountID, client.Cookies)

	// iCloud 侧删除成功后,同步清理 store 库存中的对应行,避免"幽灵行"
	// 残留并再次被分配。清理失败时向前端返回错误,提示库存未收敛。
	if cleanupErr := s.removeAliasFromInventory(c.Request.Context(), req.AccountID, anonymousID); cleanupErr != nil {
		fail(c, http.StatusInternalServerError, "别名已从 iCloud 删除,但库存清理失败: "+cleanupErr.Error())
		return
	}
	ok(c, gin.H{"anonymous_id": anonymousID})
}

// removeAliasFromInventory deletes the store inventory row matching the
// deleted iCloud alias (by account + anonymous id). Only rows with
// status='available' are removed; reserved/disabled/retired rows are always
// preserved so shipped orders and message history are never broken. When no
// fulfillment store is configured there is nothing to clean.
func (s *Server) removeAliasFromInventory(ctx context.Context, accountID, anonymousID string) error {
	if s.fulfillment == nil {
		return nil
	}
	_, err := s.fulfillment.DeleteMailboxByAnonymousID(ctx, accountID, anonymousID)
	return err
}

// isSessionError 判断错误是否由会话失效引起。
//
// 只依据明确的错误类型或状态码（hme.HTTPStatusError 的 401/403、
// ErrTrustSessionRequired），不再对自由文本做子串匹配——否则上游超时、
// 限流等无关错误会被误判为「需重新授权」，掩盖真实故障。
func isSessionError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, hme.ErrTrustSessionRequired) {
		return true
	}
	var httpErr *hme.HTTPStatusError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden
	}
	return false
}

// reloadConfig 重新加载 accounts.json 配置文件。
func (s *Server) reloadConfig(c *gin.Context) {
	if err := s.mgr.Reload(); err != nil {
		fail(c, http.StatusInternalServerError, "重新加载配置失败: "+err.Error())
		return
	}
	ok(c, gin.H{"message": "配置已重新加载"})
}

// 确保 hme 包被引用(类型在 handler 中使用)
var _ = hme.Alias{}
