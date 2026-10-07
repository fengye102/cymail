package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	mu          sync.Mutex
	path        string
	credential  *adminCredentialFile
	sessions    map[[32]byte]time.Time
	attempts    map[string]loginAttempt
	lastCleanup time.Time
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
	if !requestIP(c).IsLoopback() {
		fail(c, http.StatusForbidden, "首次设置管理员只能在服务器本机完成")
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
	setAdminSessionCookie(c, token, expiresAt)
	c.JSON(http.StatusCreated, apiResp{Success: true, Data: gin.H{"authenticated": true}})
}

func (s *Server) loginAdmin(c *gin.Context) {
	var req adminAuthRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "请输入用户名和密码")
		return
	}
	token, expiresAt, err := s.adminAuth.login(req.Username, req.Password, requestIP(c).String())
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
	setAdminSessionCookie(c, token, expiresAt)
	ok(c, gin.H{"authenticated": true})
}

func (s *Server) logoutAdmin(c *gin.Context) {
	token, _ := c.Cookie(adminSessionCookie)
	s.adminAuth.logout(token)
	http.SetCookie(c.Writer, &http.Cookie{Name: adminSessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: secureRequest(c), SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0)})
	ok(c, gin.H{"authenticated": false})
}

func setAdminSessionCookie(c *gin.Context, token string, expiresAt time.Time) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: adminSessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: secureRequest(c), SameSite: http.SameSiteStrictMode,
		Expires: expiresAt, MaxAge: int(time.Until(expiresAt).Seconds()),
	})
}

func secureRequest(c *gin.Context) bool {
	if c.Request.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(strings.Split(c.GetHeader("X-Forwarded-Proto"), ",")[0]), "https")
}

func requestIP(c *gin.Context) net.IP {
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		host = c.Request.RemoteAddr
	}
	remote := net.ParseIP(strings.Trim(host, "[]"))
	if remote != nil && remote.IsLoopback() {
		for _, header := range []string{"X-Forwarded-For", "X-Real-IP"} {
			candidate := strings.TrimSpace(strings.Split(c.GetHeader(header), ",")[0])
			if parsed := net.ParseIP(candidate); parsed != nil {
				return parsed
			}
		}
	}
	if remote == nil {
		return net.IPv4zero
	}
	return remote
}
