package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/argon2"
)

// trustedProxiesOnce 缓存 TRUSTED_PROXIES 的解析结果，避免每个请求重复解析。
var (
	trustedProxiesOnce  sync.Once
	trustedProxiesValue trustedProxyList
)

const (
	adminSessionCookie = "cymail_admin_session"
	adminSessionTTL    = 12 * time.Hour
	loginWindow        = 15 * time.Minute
	maxLoginFailures   = 5
	argonTime          = uint32(3)
	argonMemory        = uint32(32 * 1024)
	argonThreads       = uint8(2)
	argonKeyLength     = uint32(32)
)

var (
	errAdminAuthUnavailable = errors.New("管理员登录尚未配置")
	errAdminAlreadySetup    = errors.New("管理员账号已经初始化")
	errInvalidCredentials   = errors.New("用户名或密码错误")
	errTooManyAttempts      = errors.New("登录失败次数过多，请稍后重试")
)

type adminCredentialFile struct {
	Version      int       `json:"version"`
	Username     string    `json:"username"`
	Salt         string    `json:"salt"`
	PasswordHash string    `json:"password_hash"`
	ArgonTime    uint32    `json:"argon_time"`
	ArgonMemory  uint32    `json:"argon_memory"`
	ArgonThreads uint8     `json:"argon_threads"`
	CreatedAt    time.Time `json:"created_at"`
}

type loginAttempt struct {
	Failures    int
	WindowStart time.Time
	LockedUntil time.Time
}

type adminAuthStore struct {
	mu             sync.Mutex
	path           string
	credential     *adminCredentialFile
	sessions       map[[32]byte]time.Time
	attempts       map[string]loginAttempt
	lastCleanup    time.Time
	bootstrapToken string // 首次初始化管理员用的一次性引导令牌；setup 成功后清空
}

func openAdminAuthStore(path string) (*adminAuthStore, error) {
	store := &adminAuthStore{
		path:     strings.TrimSpace(path),
		sessions: make(map[[32]byte]time.Time),
		attempts: make(map[string]loginAttempt),
	}
	if store.path == "" {
		return store, nil
	}
	raw, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read admin auth file: %w", err)
	}
	var credential adminCredentialFile
	if err := json.Unmarshal(raw, &credential); err != nil {
		return nil, fmt.Errorf("decode admin auth file: %w", err)
	}
	if err := validateStoredCredential(&credential); err != nil {
		return nil, err
	}
	store.credential = &credential
	return store, nil
}

// ensureBootstrapToken 在管理员尚未初始化时生成一次性引导令牌并打印到日志，
// 让首次部署可以通过反代（而非仅限本机）完成管理员初始化。令牌只在
// 内存中保存，管理员创建成功后立即作废。
func (a *adminAuthStore) ensureBootstrapToken() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.credential != nil || a.bootstrapToken != "" {
		return
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		// 生成失败时退化为仅允许本机初始化（bootstrapAuthorized 的 loopback 分支）。
		log.Printf("WARNING: 生成管理员引导令牌失败，首次设置将只能在本机完成: %v", err)
		return
	}
	a.bootstrapToken = base64.RawURLEncoding.EncodeToString(raw)
	log.Printf("管理员尚未初始化。本次启动的引导令牌（仅在创建第一个管理员前有效）: %s", a.bootstrapToken)
	log.Printf("首次部署请在管理页创建管理员，并在请求头携带 X-Bootstrap-Token: %s", a.bootstrapToken)
}

// bootstrapTokenAllowed 校验请求携带的引导令牌。恒时比较；管理员已存在时恒为 false。
func (a *adminAuthStore) bootstrapTokenAllowed(provided string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.credential != nil || a.bootstrapToken == "" || provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a.bootstrapToken), []byte(provided)) == 1
}

func validateStoredCredential(credential *adminCredentialFile) error {
	if credential.Version != 1 || validateAdminUsername(credential.Username) != nil {
		return errors.New("admin auth file is invalid")
	}
	salt, saltErr := base64.RawStdEncoding.DecodeString(credential.Salt)
	hash, hashErr := base64.RawStdEncoding.DecodeString(credential.PasswordHash)
	if saltErr != nil || hashErr != nil || len(salt) != 16 || len(hash) != int(argonKeyLength) {
		return errors.New("admin auth file contains invalid password material")
	}
	if credential.ArgonTime == 0 || credential.ArgonMemory < 8*1024 || credential.ArgonThreads == 0 {
		return errors.New("admin auth file contains invalid Argon2 parameters")
	}
	return nil
}

func validateAdminUsername(username string) error {
	if username != strings.TrimSpace(username) || utf8.RuneCountInString(username) < 2 || utf8.RuneCountInString(username) > 64 {
		return errors.New("用户名长度需要为 2 到 64 个字符")
	}
	for _, r := range username {
		if unicode.IsControl(r) {
			return errors.New("用户名不能包含控制字符")
		}
	}
	return nil
}

func validateAdminPassword(password string) error {
	if len(password) < 12 {
		return errors.New("密码至少需要 12 个字符")
	}
	if len(password) > 256 {
		return errors.New("密码不能超过 256 个字符")
	}
	return nil
}

func (a *adminAuthStore) initialized() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.credential != nil
}

func (a *adminAuthStore) setup(username, password string) (string, time.Time, error) {
	username = strings.TrimSpace(username)
	if err := validateAdminUsername(username); err != nil {
		return "", time.Time{}, err
	}
	if err := validateAdminPassword(password); err != nil {
		return "", time.Time{}, err
	}
	if a.path == "" {
		return "", time.Time{}, errAdminAuthUnavailable
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.credential != nil {
		return "", time.Time{}, errAdminAlreadySetup
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", time.Time{}, fmt.Errorf("generate password salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLength)
	credential := &adminCredentialFile{
		Version: 1, Username: username,
		Salt: base64.RawStdEncoding.EncodeToString(salt), PasswordHash: base64.RawStdEncoding.EncodeToString(hash),
		ArgonTime: argonTime, ArgonMemory: argonMemory, ArgonThreads: argonThreads, CreatedAt: time.Now().UTC(),
	}
	if err := writeCredentialFile(a.path, credential); err != nil {
		return "", time.Time{}, err
	}
	a.credential = credential
	a.bootstrapToken = "" // 引导令牌一次性使用，管理员创建后立即作废
	return a.newSessionLocked()
}

func writeCredentialFile(path string, credential *adminCredentialFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create admin auth directory: %w", err)
	}
	raw, err := json.MarshalIndent(credential, "", "  ")
	if err != nil {
		return fmt.Errorf("encode admin auth file: %w", err)
	}
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open temporary admin auth file: %w", err)
	}
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write admin auth file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace admin auth file: %w", err)
	}
	return nil
}

func (a *adminAuthStore) login(username, password, clientKey string) (string, time.Time, error) {
	now := time.Now()
	a.mu.Lock()
	a.cleanupLocked(now)
	credential := a.credential
	if credential == nil {
		a.mu.Unlock()
		return "", time.Time{}, errAdminAuthUnavailable
	}
	attempt := a.attempts[clientKey]
	if attempt.LockedUntil.After(now) {
		a.mu.Unlock()
		return "", time.Time{}, errTooManyAttempts
	}
	credentialCopy := *credential
	a.mu.Unlock()

	salt, _ := base64.RawStdEncoding.DecodeString(credentialCopy.Salt)
	expectedHash, _ := base64.RawStdEncoding.DecodeString(credentialCopy.PasswordHash)
	providedHash := argon2.IDKey([]byte(password), salt, credentialCopy.ArgonTime, credentialCopy.ArgonMemory, credentialCopy.ArgonThreads, uint32(len(expectedHash)))
	expectedUser := sha256.Sum256([]byte(strings.ToLower(credentialCopy.Username)))
	providedUser := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(username))))
	valid := subtle.ConstantTimeCompare(expectedHash, providedHash) == 1 && subtle.ConstantTimeCompare(expectedUser[:], providedUser[:]) == 1

	a.mu.Lock()
	defer a.mu.Unlock()
	if !valid {
		a.recordFailureLocked(clientKey, now)
		if a.attempts[clientKey].LockedUntil.After(now) {
			return "", time.Time{}, errTooManyAttempts
		}
		return "", time.Time{}, errInvalidCredentials
	}
	delete(a.attempts, clientKey)
	return a.newSessionLocked()
}

func (a *adminAuthStore) recordFailureLocked(clientKey string, now time.Time) {
	attempt := a.attempts[clientKey]
	if attempt.WindowStart.IsZero() || now.Sub(attempt.WindowStart) > loginWindow {
		attempt = loginAttempt{WindowStart: now}
	}
	attempt.Failures++
	if attempt.Failures >= maxLoginFailures {
		attempt.LockedUntil = now.Add(loginWindow)
	}
	a.attempts[clientKey] = attempt
}

func (a *adminAuthStore) newSessionLocked() (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("generate admin session: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	expiresAt := time.Now().Add(adminSessionTTL)
	if len(a.sessions) >= 64 {
		var oldestHash [32]byte
		oldestTime := expiresAt
		for candidate, candidateExpiry := range a.sessions {
			if candidateExpiry.Before(oldestTime) {
				oldestHash, oldestTime = candidate, candidateExpiry
			}
		}
		delete(a.sessions, oldestHash)
	}
	a.sessions[hash] = expiresAt
	return token, expiresAt, nil
}

func (a *adminAuthStore) validateSession(token string) bool {
	if token == "" {
		return false
	}
	hash := sha256.Sum256([]byte(token))
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cleanupLocked(now)
	expiresAt, exists := a.sessions[hash]
	return exists && expiresAt.After(now)
}

func (a *adminAuthStore) logout(token string) {
	if token == "" {
		return
	}
	hash := sha256.Sum256([]byte(token))
	a.mu.Lock()
	delete(a.sessions, hash)
	a.mu.Unlock()
}

func (a *adminAuthStore) cleanupLocked(now time.Time) {
	if !a.lastCleanup.IsZero() && now.Sub(a.lastCleanup) < time.Minute {
		return
	}
	for hash, expiresAt := range a.sessions {
		if !expiresAt.After(now) {
			delete(a.sessions, hash)
		}
	}
	for key, attempt := range a.attempts {
		if !attempt.LockedUntil.After(now) && now.Sub(attempt.WindowStart) > loginWindow {
			delete(a.attempts, key)
		}
	}
	a.lastCleanup = now
}

type adminAuthRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

func (s *Server) adminAuthStatus(c *gin.Context) {
	token, _ := c.Cookie(adminSessionCookie)
	ok(c, gin.H{"initialized": s.adminAuth.initialized(), "authenticated": s.adminAuth.validateSession(token)})
}

func (s *Server) setupAdmin(c *gin.Context) {
	if !s.bootstrapAuthorized(c) {
		fail(c, http.StatusForbidden, "首次设置管理员需要在服务器本机操作，或携带启动日志中的引导令牌（请求头 X-Bootstrap-Token）")
		return
	}
	var req adminAuthRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "请输入管理员用户名和密码")
		return
	}
	token, expiresAt, err := s.adminAuth.setup(req.Username, req.Password)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errAdminAlreadySetup) {
			code = http.StatusConflict
		}
		fail(c, code, err.Error())
		return
	}
	setAdminSessionCookie(c, s, token, expiresAt)
	c.JSON(http.StatusCreated, apiResp{Success: true, Data: gin.H{"authenticated": true}})
}

func (s *Server) loginAdmin(c *gin.Context) {
	var req adminAuthRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "请输入用户名和密码")
		return
	}
	token, expiresAt, err := s.adminAuth.login(req.Username, req.Password, adminLoginClientKey(req.Username, requestIP(c)))
	if err != nil {
		code := http.StatusUnauthorized
		if errors.Is(err, errAdminAuthUnavailable) {
			code = http.StatusConflict
		} else if errors.Is(err, errTooManyAttempts) {
			code = http.StatusTooManyRequests
		}
		fail(c, code, err.Error())
		return
	}
	setAdminSessionCookie(c, s, token, expiresAt)
	ok(c, gin.H{"authenticated": true})
}

func (s *Server) logoutAdmin(c *gin.Context) {
	token, _ := c.Cookie(adminSessionCookie)
	s.adminAuth.logout(token)
	http.SetCookie(c.Writer, &http.Cookie{Name: adminSessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.secureRequest(c), SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0)})
	ok(c, gin.H{"authenticated": false})
}

func setAdminSessionCookie(c *gin.Context, s *Server, token string, expiresAt time.Time) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: adminSessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: s.secureRequest(c), SameSite: http.SameSiteStrictMode,
		Expires: expiresAt, MaxAge: int(time.Until(expiresAt).Seconds()),
	})
}

// secureRequest 判断当前请求是否走 HTTPS：
//   - 直连 TLS；
//   - 直连对端是受信代理且 X-Forwarded-Proto 为 https；
//   - 服务配置了 HTTPS 的 PUBLIC_BASE_URL（生产双域名拓扑统一强制 Secure，
//     避免依赖转发头是否被正确携带）。
func (s *Server) secureRequest(c *gin.Context) bool {
	if c.Request.TLS != nil {
		return true
	}
	if strings.HasPrefix(s.publicBaseURL, "https://") {
		return true
	}
	// X-Forwarded-Proto 只在直连对端是受信代理时才采信，防止直连伪造。
	if !directPeerTrusted(c) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(strings.Split(c.GetHeader("X-Forwarded-Proto"), ",")[0]), "https")
}

// bootstrapAuthorized 决定首次设置管理员是否放行：
//   - 请求携带引导令牌（X-Bootstrap-Token，值在服务首次启动、管理员未初始化时打印到日志），或
//   - 直连对端是本机 loopback（纯本机部署、无反向代理的场景）。
//
// 引导令牌只允许使用一次，管理员初始化完成后立即作废。
func (s *Server) bootstrapAuthorized(c *gin.Context) bool {
	if s.adminAuth.bootstrapTokenAllowed(strings.TrimSpace(c.GetHeader("X-Bootstrap-Token"))) {
		return true
	}
	if ip := requestIP(c); ip.IsLoopback() && directPeerTrusted(c) {
		return true
	}
	return false
}

// adminLoginClientKey 生成登录失败限速的桶键：用户名 + 真实来源 IP。
// 两者组合可以同时防住「同一 IP 撞多个用户名」和「分布式撞同一用户名」，
// 避免退化为全局单桶后任何人都能锁死全体管理员。
func adminLoginClientKey(username string, ip net.IP) string {
	return strings.ToLower(strings.TrimSpace(username)) + "|" + ip.String()
}

// directPeerTrusted 判断请求的直连对端（TCP 层对端）是否属于受信代理。
// 受信代理列表来自环境变量 TRUSTED_PROXIES（逗号分隔 CIDR 或 IP），
// 未配置时默认信任 Docker 内网段与本机，使 api 在容器拓扑下能正确
// 识别经 nginx/Caddy 转发而来的真实客户端 IP。
func directPeerTrusted(c *gin.Context) bool {
	return trustedProxyCIDRs().contains(requestPeerIP(c))
}

type trustedProxyList []*net.IPNet

func trustedProxyCIDRs() trustedProxyList {
	trustedProxiesOnce.Do(func() {
		raw := strings.TrimSpace(os.Getenv("TRUSTED_PROXIES"))
		var list trustedProxyList
		if raw == "" {
			// 默认：本机回环 + Docker 默认网桥/自定义网络常用网段 +
			// Compose 内部网络默认网段。覆盖标准容器部署拓扑。
			for _, cidr := range []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
				if _, ipnet, err := net.ParseCIDR(cidr); err == nil {
					list = append(list, ipnet)
				}
			}
		} else {
			for _, part := range strings.Split(raw, ",") {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}
				if !strings.Contains(part, "/") {
					if ip := net.ParseIP(part); ip != nil {
						bits := 32
						if ip.To4() == nil {
							bits = 128
						}
						part = fmt.Sprintf("%s/%d", ip.String(), bits)
					} else {
						continue
					}
				}
				if _, ipnet, err := net.ParseCIDR(part); err == nil {
					list = append(list, ipnet)
				}
			}
		}
		trustedProxiesValue = list
	})
	return trustedProxiesValue
}

func (l trustedProxyList) contains(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, ipnet := range l {
		if ipnet.Contains(ip) {
			return true
		}
	}
	return false
}

// requestPeerIP 返回 TCP 直连对端 IP（不做任何转发头解析）。
func requestPeerIP(c *gin.Context) net.IP {
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		host = c.Request.RemoteAddr
	}
	return net.ParseIP(strings.Trim(host, "[]"))
}

// requestIP 返回请求的真实来源 IP：
//   - 直连对端属于受信代理时，取 X-Forwarded-For 链中从右往左第一个
//     不在受信列表内的地址（即代理追加的客户端真实 IP）；
//   - 否则一律以直连对端为准，忽略转发头（不可信，可伪造）。
func requestIP(c *gin.Context) net.IP {
	peer := requestPeerIP(c)
	if peer == nil {
		return net.IPv4zero
	}
	if !trustedProxyCIDRs().contains(peer) {
		return peer
	}
	xff := c.GetHeader("X-Forwarded-For")
	if xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			candidate := net.ParseIP(strings.TrimSpace(parts[i]))
			if candidate == nil {
				continue
			}
			if trustedProxyCIDRs().contains(candidate) {
				continue
			}
			return candidate
		}
	}
	if candidate := net.ParseIP(strings.TrimSpace(c.GetHeader("X-Real-IP"))); candidate != nil && !trustedProxyCIDRs().contains(candidate) {
		return candidate
	}
	return peer
}
