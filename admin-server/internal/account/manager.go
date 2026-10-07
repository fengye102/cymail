// Package account 实现多账号管理器。
//
// 负责账号 CRUD、Cookie 解析(Header String / JSON)、持久化到 accounts.json,
// 以及创建 HME 客户端和邮件客户端。对应原 Python 项目 account_manager.py。
package account

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
)

// Account 描述一个 iCloud 账号。
type Account struct {
	ID                string                     `json:"id"`
	Name              string                     `json:"name"`
	RealEmail         string                     `json:"real_email"`
	ICloudEmail       string                     `json:"icloud_email"`
	Cookies           map[string]string          `json:"cookies"`
	Host              string                     `json:"host"`
	Proxy             string                     `json:"proxy,omitempty"` // HTTP/SOCKS5 代理
	AppPassword       string                     `json:"app_password,omitempty"`
	ForwardToEmails   []string                   `json:"forward_to_emails,omitempty"`
	SelectedForwardTo string                     `json:"selected_forward_to,omitempty"`
	ForwardMailboxes  map[string]*ForwardMailbox `json:"forward_mailboxes,omitempty"`
	AppleAccount      *hme.AppleAccountState     `json:"apple_account,omitempty"` // 新接口 (Apple Account 管理) 会话
	Status            string                     `json:"status"`                  // active / error
	AliasTotal        int                        `json:"alias_total"`
	AliasActive       int                        `json:"alias_active"`
	LastValidated     string                     `json:"last_validated"`
	LastError         string                     `json:"last_error,omitempty"`
	CreatedAt         string                     `json:"created_at"`
}

// ForwardMailbox stores an authorized webmail session for one Apple forwarding target.
// Reusable browser credentials are encrypted together with accounts.json and are
// removed from every API response by Account.Redacted.
type ForwardMailbox struct {
	Email         string            `json:"email"`
	Provider      string            `json:"provider"`
	WebHost       string            `json:"web_host"`
	Cookies       map[string]string `json:"cookies,omitempty"`
	SessionID     string            `json:"session_id,omitempty"`
	Authorized    bool              `json:"authorized"`
	LastValidated string            `json:"last_validated,omitempty"`
	LastError     string            `json:"last_error,omitempty"`
}

// Redacted returns an API-safe copy with every reusable credential removed.
func (a *Account) Redacted() *Account {
	if a == nil {
		return nil
	}
	cp := *a
	cp.Cookies = nil
	cp.AppPassword = ""
	cp.Proxy = ""
	cp.AppleAccount = a.AppleAccount.Redacted()
	cp.ForwardMailboxes = cloneForwardMailboxes(a.ForwardMailboxes)
	for _, mailbox := range cp.ForwardMailboxes {
		mailbox.Cookies = nil
		mailbox.SessionID = ""
	}
	return &cp
}

// AppleAccountStatus 返回新接口会话的公开状态 (不含任何凭证)。
func (a *Account) AppleAccountStatus() map[string]any {
	if a == nil || a.AppleAccount == nil {
		return map[string]any{"enabled": false}
	}
	state := a.AppleAccount.Redacted()
	return map[string]any{
		"enabled":           true,
		"last_checked_at":   state.LastCheckedAt,
		"last_check_ok":     state.LastCheckOK,
		"last_status":       state.LastStatus,
		"manage_expires_at": state.ManageExpiresAt,
		"saved_at":          state.SavedAt,
	}
}

// Manager 管理多个 iCloud 账号,线程安全。
type Manager struct {
	mu       sync.RWMutex
	accounts map[string]*Account
	dataDir  string
	dataFile string
	dataKey  []byte
}

const encryptedDataPrefix = "ICLOUD-HME-ENC-V1\n"

// NewManager 创建管理器。dataDir 用于存放 accounts.json。
func NewManager(dataDir string) (*Manager, error) {
	return newManager(dataDir, nil)
}

// NewManagerWithKey encrypts accounts.json using a base64-encoded 32-byte key.
func NewManagerWithKey(dataDir, encodedKey string) (*Manager, error) {
	key, err := decodeDataKey(encodedKey)
	if err != nil {
		return nil, err
	}
	return newManager(dataDir, key)
}

func newManager(dataDir string, dataKey []byte) (*Manager, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	_ = os.Chmod(dataDir, 0700)
	m := &Manager{
		accounts: make(map[string]*Account),
		dataDir:  dataDir,
		dataFile: filepath.Join(dataDir, "accounts.json"),
		dataKey:  dataKey,
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	// 启动即备份：服务启动时执行一次每日备份（幂等，同一天只备份一次）。
	if err := m.backupAccountsDaily(); err != nil {
		log.Printf("启动时 accounts.json 每日备份失败: %v", err)
	}
	return m, nil
}

// Reload 重新加载 accounts.json 配置文件。
func (m *Manager) Reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.load()
}

func (m *Manager) load() error {
	raw, err := os.ReadFile(m.dataFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	_ = os.Chmod(m.dataFile, 0600)
	if bytes.HasPrefix(raw, []byte(encryptedDataPrefix)) {
		if len(m.dataKey) == 0 {
			return fmt.Errorf("accounts.json is encrypted; configure ICLOUD_HME_DATA_KEY")
		}
		raw, err = decryptData(raw[len(encryptedDataPrefix):], m.dataKey)
		if err != nil {
			return fmt.Errorf("decrypt accounts.json: %w", err)
		}
	}
	var wrapper struct {
		Accounts map[string]*Account `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return err
	}
	m.accounts = wrapper.Accounts
	if m.accounts == nil {
		m.accounts = make(map[string]*Account)
	}
	for id, acc := range m.accounts {
		if acc == nil {
			return fmt.Errorf("account %s is null", id)
		}
		host, err := normalizeICloudHost(acc.Host)
		if err != nil {
			return fmt.Errorf("account %s: %w", id, err)
		}
		acc.Host = host
		if acc.ForwardMailboxes == nil {
			acc.ForwardMailboxes = make(map[string]*ForwardMailbox)
		}
		for email, mailbox := range acc.ForwardMailboxes {
			if mailbox == nil {
				continue
			}
			provider, webHost := webmailProvider(email)
			mailbox.Email = normalizeEmail(firstNonEmpty(mailbox.Email, email))
			mailbox.Provider = provider
			mailbox.WebHost = webHost
			// Versions before 1.3 stored IMAP credentials here. Never treat an
			// old IMAP authorization flag as a valid browser session.
			if len(mailbox.Cookies) == 0 || mailbox.SessionID == "" {
				mailbox.Authorized = false
			}
		}
	}
	return nil
}

func (m *Manager) save() error {
	wrapper := struct {
		Accounts  map[string]*Account `json:"accounts"`
		UpdatedAt string              `json:"updated_at"`
	}{
		Accounts:  m.accounts,
		UpdatedAt: time.Now().Format(time.RFC3339),
	}
	raw, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		return err
	}
	if len(m.dataKey) > 0 {
		raw, err = encryptData(raw, m.dataKey)
		if err != nil {
			return err
		}
	}
	if err := os.WriteFile(m.dataFile, raw, 0600); err != nil {
		return err
	}
	// 写时每日备份：幂等，同一天只生成一份备份；备份失败只记录日志，
	// 不影响主数据写入。本函数不依赖 m.mu，调用方持锁时调用无死锁风险。
	if err := m.backupAccountsDaily(); err != nil {
		log.Printf("accounts.json 每日备份失败: %v", err)
	}
	return nil
}

// backupAccountsDaily 把 accounts.json 每日备份到 <dataDir>/backups/accounts-YYYYMMDD.json。
//
// 幂等：同一天目标文件已存在则跳过复制；无论是否复制都执行保留最近 7 份的清理。
// 备份文件以 0600 落盘（临时文件 + rename，避免半截备份）；备份目录权限 0700。
func (m *Manager) backupAccountsDaily() error {
	raw, err := os.ReadFile(m.dataFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 尚无数据文件，无需备份
		}
		return fmt.Errorf("读取 accounts.json 备份源: %w", err)
	}
	backupsDir := filepath.Join(m.dataDir, "backups")
	if err := os.MkdirAll(backupsDir, 0700); err != nil {
		return fmt.Errorf("创建备份目录: %w", err)
	}
	_ = os.Chmod(backupsDir, 0700)
	const prefix = "accounts-"
	target := filepath.Join(backupsDir, prefix+time.Now().Format("20060102")+".json")
	if _, err := os.Stat(target); err == nil {
		// 幂等：同一天已备份过，仍执行清理
		return pruneDailyBackups(backupsDir, prefix, 7)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(backupsDir, ".accounts-backup-*.tmp")
	if err != nil {
		return fmt.Errorf("创建备份临时文件: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(raw); err != nil {
		return fmt.Errorf("写入备份临时文件: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		return fmt.Errorf("设置备份临时文件权限: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("同步备份临时文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭备份临时文件: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("提交备份文件: %w", err)
	}
	committed = true
	return pruneDailyBackups(backupsDir, prefix, 7)
}

// pruneDailyBackups 清理备份目录中 <prefix>YYYYMMDD.json 命名的备份，
// 仅保留最近 keep 份（按文件名日期排序，YYYYMMDD 字典序即时间序）。
func pruneDailyBackups(dir, prefix string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		datePart := name[len(prefix) : len(name)-len(".json")]
		if len(datePart) != 8 {
			continue
		}
		if _, err := time.Parse("20060102", datePart); err != nil {
			continue
		}
		names = append(names, name)
	}
	if len(names) <= keep {
		return nil
	}
	sort.Strings(names)
	for _, name := range names[:len(names)-keep] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("清理旧备份 %s: %w", name, err)
		}
	}
	return nil
}

// ParseCookieInput 解析 Cookie 输入,支持两种格式:
//   - Header String: "name1=value1; name2=value2; ..."
//   - JSON: {"name1":"value1","name2":"value2"}
//
// 空输入返回错误。
func ParseCookieInput(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("空白输入 — 请粘贴 Cookie Header String 或 JSON")
	}

	// JSON 格式
	if strings.HasPrefix(raw, "{") {
		var cookies map[string]string
		if err := json.Unmarshal([]byte(raw), &cookies); err == nil && cookies != nil {
			out := make(map[string]string, len(cookies))
			for k, v := range cookies {
				if v != "" {
					out[k] = v
				}
			}
			if len(out) > 0 {
				return out, nil
			}
		}
	}

	// Header String 格式
	cookies := make(map[string]string)
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		idx := strings.Index(part, "=")
		if idx <= 0 {
			continue
		}
		name := strings.TrimSpace(part[:idx])
		value := strings.TrimSpace(part[idx+1:])
		if name != "" {
			cookies[name] = value
		}
	}
	if len(cookies) == 0 {
		return nil, fmt.Errorf("无法解析 Cookie 输入,请提供 Header String 或 JSON 格式")
	}
	return cookies, nil
}

// AddAccount 添加一个账号。cookieInput 可为空,后续可通过 /login 获取。
//
// cookieInput 支持 Header String 或 JSON。校验失败仍会保存账号(status=error),
// 方便用户后续修正 Cookie 后重新校验。
func (m *Manager) AddAccount(name, cookieInput, host, proxy string) (*Account, error) {
	var cookies map[string]string
	if cookieInput != "" {
		var err error
		cookies, err = ParseCookieInput(cookieInput)
		if err != nil {
			return nil, err
		}
	} else {
		cookies = make(map[string]string)
	}
	host, err := normalizeICloudHost(host)
	if err != nil {
		return nil, err
	}

	acc := &Account{
		ID:               "acc_" + uuid.New().String()[:8],
		Name:             name,
		Cookies:          cookies,
		Host:             host,
		Proxy:            proxy,
		Status:           "pending", // 无 Cookie 时为 pending
		ForwardMailboxes: make(map[string]*ForwardMailbox),
		CreatedAt:        time.Now().Format(time.RFC3339),
	}

	// 有 Cookie 才校验会话
	if len(cookies) > 0 {
		client, err := hme.NewClient(cookies, host, proxy, false)
		if err != nil {
			return nil, err
		}
		if err := client.ValidateSession(); err != nil {
			acc.Status = "error"
			acc.LastError = truncate(err.Error(), 300)
		} else {
			acc.Status = "active"
			if info := client.AccountInfo(); info != nil {
				acc.RealEmail = firstNonEmpty(info.AppleID, info.PrimaryEmail)
				acc.ICloudEmail = deriveICloudEmail(info)
			}
			if aliases, err := client.ListAliases(); err == nil {
				acc.AliasTotal = len(aliases)
				for _, a := range aliases {
					if a.Active {
						acc.AliasActive++
					}
				}
			}
			acc.LastValidated = time.Now().Format(time.RFC3339)
		}
	}

	m.mu.Lock()
	m.accounts[acc.ID] = acc
	saveErr := m.save()
	m.mu.Unlock()
	if saveErr != nil {
		return nil, saveErr
	}
	return acc.Redacted(), nil
}

// RemoveAccount 删除账号。
func (m *Manager) RemoveAccount(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.accounts[id]; !ok {
		return false
	}
	delete(m.accounts, id)
	_ = m.save()
	return true
}

// GetAccount 返回账号副本。
func (m *Manager) GetAccount(id string) (*Account, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	acc, ok := m.accounts[id]
	if !ok {
		return nil, false
	}
	return cloneAccount(acc), true
}

// ListAccounts 返回所有账号(脱敏,不含 Cookies),按活跃状态排序。
func (m *Manager) ListAccounts() []*Account {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Account, 0, len(m.accounts))
	for _, acc := range m.accounts {
		out = append(out, acc.Redacted())
	}
	return out
}

// HMEClient 为指定账号创建一个新的 HME 客户端。
// 必须有有效的 Cookie 才能使用 HME 功能。
func (m *Manager) HMEClient(id string, verbose bool) (*hme.Client, error) {
	m.mu.RLock()
	acc, ok := m.accounts[id]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	cookies := cloneStringMap(acc.Cookies)
	host := acc.Host
	proxy := acc.Proxy
	m.mu.RUnlock()
	if len(cookies) == 0 {
		return nil, fmt.Errorf("账号未配置 Cookie，无法使用 HME 功能")
	}
	return hme.NewClient(cookies, host, proxy, verbose)
}

// HMEClientWithPassword 为指定账号创建一个新的 HME 客户端,使用账号密码登录。
// 登录成功后会自动获取 Cookie 并保存到账号配置。
func (m *Manager) HMEClientWithPassword(id, password string, otpProvider hme.OTPProvider) (*hme.Client, error) {
	m.mu.RLock()
	acc, ok := m.accounts[id]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf("账号不存在: %s", id)
	}

	email := acc.ICloudEmail
	if email == "" {
		email = acc.RealEmail
	}
	host := acc.Host
	proxy := acc.Proxy
	m.mu.RUnlock()
	if email == "" {
		return nil, fmt.Errorf("账号未设置邮箱地址")
	}

	client, err := hme.NewClient(nil, host, proxy, false)
	if err != nil {
		return nil, err
	}

	if err := client.Login(email, password, otpProvider); err != nil {
		return nil, err
	}

	// 保存登录后的 Cookie 到账号
	m.mu.Lock()
	current, ok := m.accounts[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	current.Cookies = cloneStringMap(client.Cookies)
	err = m.save()
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}

	return client, nil
}

// MailClient 为指定账号创建 IMAP 邮件客户端。
// 需要事先设置 iCloud 邮箱和 App 专用密码。
func (m *Manager) MailClient(id string) (*mail.Client, error) {
	m.mu.RLock()
	acc, ok := m.accounts[id]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	imapEmail := acc.ICloudEmail
	if imapEmail == "" {
		imapEmail = acc.RealEmail
	}
	appPassword := acc.AppPassword
	m.mu.RUnlock()
	if !isICloudDomain(imapEmail) {
		return nil, fmt.Errorf("账号未设置 iCloud 邮箱 (当前: %s)", imapEmail)
	}
	if appPassword == "" {
		return nil, fmt.Errorf("账号未设置 App 专用密码")
	}
	return mail.NewClient(imapEmail, appPassword), nil
}

// RefreshForwarding loads Apple's forwarding choices and persists a safe snapshot.
func (m *Manager) RefreshForwarding(id string) (*hme.ForwardingSettings, error) {
	client, err := m.HMEClient(id, false)
	if err != nil {
		return nil, err
	}
	settings, err := client.ListForwardingSettings()
	if err != nil {
		_ = m.SaveCookies(id, client.Cookies)
		return nil, err
	}
	if err := m.saveForwardingSnapshot(id, settings, client.Cookies); err != nil {
		return nil, err
	}
	return settings, nil
}

// UpdateDefaultForwardTo changes Apple's selected target after checking it is offered by Apple.
func (m *Manager) UpdateDefaultForwardTo(id, email string) (*hme.ForwardingSettings, error) {
	email = normalizeEmail(email)
	client, err := m.HMEClient(id, false)
	if err != nil {
		return nil, err
	}
	settings, err := client.ListForwardingSettings()
	if err != nil {
		return nil, err
	}
	found := false
	for _, candidate := range settings.ForwardToEmails {
		if strings.EqualFold(candidate, email) {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("该邮箱不在 Apple 的转发候选列表中")
	}
	if err := client.UpdateForwardTo(email); err != nil {
		_ = m.SaveCookies(id, client.Cookies)
		return nil, err
	}
	settings, err = client.ListForwardingSettings()
	if err != nil {
		return nil, err
	}
	if err := m.saveForwardingSnapshot(id, settings, client.Cookies); err != nil {
		return nil, err
	}
	return settings, nil
}

// AuthorizeForwardWebMailbox verifies a browser session through the provider's
// harmless mailbox-list endpoint before persisting it.
func (m *Manager) AuthorizeForwardWebMailbox(id string, cfg ForwardMailbox) error {
	cfg.Email = normalizeEmail(cfg.Email)
	if cfg.Email == "" || !strings.Contains(cfg.Email, "@") {
		return fmt.Errorf("转发邮箱格式错误")
	}
	provider, webHost := webmailProvider(cfg.Email)
	if provider == "" {
		return fmt.Errorf("目前仅支持 163 邮箱网页登录取件")
	}
	cfg.Provider = provider
	cfg.WebHost = webHost
	cfg.SessionID = strings.TrimSpace(cfg.SessionID)
	if len(cfg.Cookies) == 0 || cfg.SessionID == "" {
		return fmt.Errorf("163 网页会话不完整，请在 163 官方邮箱页面重新授权")
	}
	m.mu.RLock()
	acc, ok := m.accounts[id]
	allowed := false
	if ok {
		for _, candidate := range acc.ForwardToEmails {
			if strings.EqualFold(candidate, cfg.Email) {
				allowed = true
				break
			}
		}
	}
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	if !allowed {
		return fmt.Errorf("请先刷新 Apple 转发邮箱列表")
	}
	mc, err := mail.NewNeteaseWebClient(cfg.Email, cfg.Cookies, cfg.SessionID, cfg.WebHost)
	if err != nil {
		m.recordForwardingError(id, cfg, err)
		return err
	}
	if err := mc.ValidateSession(); err != nil {
		m.recordForwardingError(id, cfg, err)
		return err
	}
	cfg.Authorized = true
	cfg.LastValidated = time.Now().Format(time.RFC3339)
	cfg.LastError = ""
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok = m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	if acc.ForwardMailboxes == nil {
		acc.ForwardMailboxes = make(map[string]*ForwardMailbox)
	}
	copy := cfg
	acc.ForwardMailboxes[cfg.Email] = &copy
	return m.save()
}

// ForwardWebMailClient returns only a previously verified webmail client.
func (m *Manager) ForwardWebMailClient(id, email string) (*mail.NeteaseWebClient, error) {
	email = normalizeEmail(email)
	m.mu.RLock()
	acc, ok := m.accounts[id]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	cfg := acc.ForwardMailboxes[email]
	if cfg != nil {
		copy := *cfg
		cfg = &copy
	}
	m.mu.RUnlock()
	if cfg == nil || !cfg.Authorized || len(cfg.Cookies) == 0 || cfg.SessionID == "" {
		return nil, fmt.Errorf("转发目标 %s 尚未完成收件授权", email)
	}
	if cfg.Provider != "netease_163" {
		return nil, fmt.Errorf("转发目标 %s 暂不支持网页登录取件", email)
	}
	return mail.NewNeteaseWebClient(cfg.Email, cfg.Cookies, cfg.SessionID, cfg.WebHost)
}

// WebMailClient 为指定账号创建 Web 邮件客户端。
// 使用 Cookie 认证，无需 App Password。
func (m *Manager) WebMailClient(id string) (*mail.WebClient, error) {
	m.mu.RLock()
	acc, ok := m.accounts[id]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	cookies := cloneStringMap(acc.Cookies)
	host := acc.Host
	m.mu.RUnlock()
	if len(cookies) == 0 {
		return nil, fmt.Errorf("账号未配置 Cookie，无法读取邮件")
	}
	// 从 cookies 中获取 dsid
	dsid := ""
	if v, ok := cookies["X-APPLE-WEBAUTH-USER"]; ok {
		// 解析 "v=1:s=1:d=22789132008" 格式
		parts := strings.Split(v, ":d=")
		if len(parts) == 2 {
			dsid = parts[1]
		}
	}
	return mail.NewWebClient(cookies, dsid, host), nil
}

// SetAppPassword 设置 iCloud 邮箱和 App 专用密码,并测试 IMAP 连接。
func (m *Manager) SetAppPassword(id, icloudEmail, appPassword string) error {
	m.mu.RLock()
	_, ok := m.accounts[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	if icloudEmail == "" {
		return fmt.Errorf("iCloud 邮箱不能为空")
	}
	if appPassword == "" {
		return fmt.Errorf("App 专用密码不能为空")
	}

	// 测试连接
	mc := mail.NewClient(icloudEmail, appPassword)
	if err := mc.Connect(); err != nil {
		return err
	}
	count, err := mc.InboxCount()
	mc.Disconnect()
	if err != nil {
		return err
	}

	m.mu.Lock()
	acc, ok := m.accounts[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("账号不存在: %s", id)
	}
	acc.ICloudEmail = icloudEmail
	acc.AppPassword = appPassword
	err = m.save()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	_ = count
	return nil
}

// SaveCookies 保存指定账号的最新 Cookie（HMEClient 操作后刷新的 token）。
// 用于客户端 validate/操作过程中从 Set-Cookie 获取了新 token 后持久化。
func (m *Manager) SaveCookies(id string, cookies map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	acc.Cookies = cloneStringMap(cookies)
	return m.save()
}

// UpdateCookies 更新指定账号的 Cookie,并自动校验会话有效性。
func (m *Manager) UpdateCookies(id string, cookies map[string]string) error {
	if len(cookies) == 0 {
		return fmt.Errorf("cookies 不能为空")
	}
	m.mu.RLock()
	acc, ok := m.accounts[id]
	if !ok {
		m.mu.RUnlock()
		return fmt.Errorf("账号不存在: %s", id)
	}
	host := acc.Host
	proxy := acc.Proxy
	m.mu.RUnlock()

	// 自动校验 Cookie 是否有效
	if host == "" {
		host = "icloud.com"
	}
	client, err := hme.NewClient(cloneStringMap(cookies), host, proxy, false)
	if err != nil {
		m.mu.Lock()
		if current, exists := m.accounts[id]; exists {
			current.Cookies = cloneStringMap(cookies)
			current.Status = "error"
			current.LastError = "创建客户端失败: " + err.Error()
		}
		_ = m.save()
		m.mu.Unlock()
		return err
	}
	status := "active"
	lastError := ""
	lastValidated := time.Now().Format(time.RFC3339)
	var realEmail, icloudEmail string
	var validationErr error
	if err := client.ValidateSession(); err != nil {
		validationErr = err
		status = "error"
		lastError = "Cookie 校验失败: " + err.Error()
		lastValidated = ""
	} else {
		if info := client.AccountInfo(); info != nil {
			realEmail = firstNonEmpty(info.AppleID, info.PrimaryEmail)
			icloudEmail = deriveICloudEmail(info)
		}
	}

	m.mu.Lock()
	current, ok := m.accounts[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("账号不存在: %s", id)
	}
	current.Cookies = cloneStringMap(client.Cookies)
	current.Host = host
	current.Status = status
	current.LastError = lastError
	if lastValidated != "" {
		current.LastValidated = lastValidated
	}
	if realEmail != "" {
		current.RealEmail = realEmail
	}
	if current.ICloudEmail == "" && icloudEmail != "" {
		current.ICloudEmail = icloudEmail
	}
	saveErr := m.save()
	m.mu.Unlock()
	if saveErr != nil {
		return saveErr
	}
	return validationErr
}

// ---- Apple Account 管理接口 (新接口) 会话 ----

// SetAppleAccount 导入 account.apple.com 浏览器 Cookie 并引导出新接口会话。
//
// 引导成功后保存会话; 引导失败返回错误且不保存 (旧接口不受影响)。
// cookies 必须来自已登录的 account.apple.com 浏览器会话。
func (m *Manager) SetAppleAccount(id string, cookies map[string]string) (*hme.AppleAccountClient, error) {
	if len(cookies) == 0 {
		return nil, fmt.Errorf("Apple Account Cookie 不能为空")
	}
	m.mu.RLock()
	_, ok := m.accounts[id]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	m.mu.RUnlock()

	state := hme.AppleAccountState{
		Cookies:   cloneStringMap(cookies),
		SavedAt:   time.Now(),
		Origin:    hme.AppleAccountManageOrigin(),
		UserAgent: hme.AppleAccountManageUserAgent(),
	}
	client := hme.NewAppleAccountClient(state, false)
	if err := client.Bootstrap(); err != nil {
		return nil, fmt.Errorf("Apple Account 会话引导失败 (请确认已在浏览器登录 account.apple.com 并重新采集): %w", err)
	}

	m.mu.Lock()
	current, ok := m.accounts[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	next := client.State()
	current.AppleAccount = &next
	saveErr := m.save()
	m.mu.Unlock()
	if saveErr != nil {
		return nil, saveErr
	}
	return client, nil
}

// ClearAppleAccount 清除指定账号的新接口会话, 只保留旧接口。
func (m *Manager) ClearAppleAccount(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	acc.AppleAccount = nil
	return m.save()
}

// AppleAccountClient 为指定账号创建新接口客户端。
// 账号未配置新接口会话时返回错误。
func (m *Manager) AppleAccountClient(id string) (*hme.AppleAccountClient, error) {
	m.mu.RLock()
	acc, ok := m.accounts[id]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	state := acc.AppleAccount.Clone()
	m.mu.RUnlock()
	if len(state.Cookies) == 0 {
		return nil, fmt.Errorf("账号未配置 Apple Account 管理会话 (新接口), 创建将回退旧接口")
	}
	return hme.NewAppleAccountClient(state, false), nil
}

// SaveAppleAccountState 持久化新接口客户端的最新会话状态 (scnt/apiKey/Cookie 会轮换)。
func (m *Manager) SaveAppleAccountState(id string, state hme.AppleAccountState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	next := state.Clone()
	acc.AppleAccount = &next
	return m.save()
}

// AppleAccountStates 返回所有已配置新接口会话的账号及其会话副本。
// 仅供服务内部使用 (保活循环等), 返回的是深拷贝, 修改不影响存储。
func (m *Manager) AppleAccountStates() map[string]hme.AppleAccountState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]hme.AppleAccountState)
	for id, acc := range m.accounts {
		if acc.AppleAccount != nil {
			out[id] = acc.AppleAccount.Clone()
		}
	}
	return out
}

// ---- 辅助函数 ----

func decodeDataKey(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, fmt.Errorf("data encryption key is empty")
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("data encryption key must be base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("data encryption key must decode to exactly 32 bytes")
	}
	return key, nil
}

func encryptData(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	encoded := base64.StdEncoding.EncodeToString(sealed)
	return []byte(encryptedDataPrefix + encoded), nil
}

func decryptData(encoded, key []byte) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, fmt.Errorf("encrypted payload is truncated")
	}
	nonce, ciphertext := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func cloneStringMap(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func cloneForwardMailboxes(src map[string]*ForwardMailbox) map[string]*ForwardMailbox {
	if src == nil {
		return nil
	}
	dst := make(map[string]*ForwardMailbox, len(src))
	for key, value := range src {
		if value != nil {
			copy := *value
			copy.Cookies = cloneStringMap(value.Cookies)
			dst[key] = &copy
		}
	}
	return dst
}

func cloneAccount(src *Account) *Account {
	if src == nil {
		return nil
	}
	cp := *src
	cp.Cookies = cloneStringMap(src.Cookies)
	cp.ForwardToEmails = append([]string(nil), src.ForwardToEmails...)
	cp.ForwardMailboxes = cloneForwardMailboxes(src.ForwardMailboxes)
	if src.AppleAccount != nil {
		state := src.AppleAccount.Clone()
		cp.AppleAccount = &state
	}
	return &cp
}

func (m *Manager) saveForwardingSnapshot(id string, settings *hme.ForwardingSettings, cookies map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	acc.Cookies = cloneStringMap(cookies)
	acc.SelectedForwardTo = normalizeEmail(settings.SelectedForwardTo)
	acc.ForwardToEmails = make([]string, 0, len(settings.ForwardToEmails))
	if acc.ForwardMailboxes == nil {
		acc.ForwardMailboxes = make(map[string]*ForwardMailbox)
	}
	seen := make(map[string]bool)
	for _, candidate := range settings.ForwardToEmails {
		email := normalizeEmail(candidate)
		if email == "" || seen[email] {
			continue
		}
		seen[email] = true
		acc.ForwardToEmails = append(acc.ForwardToEmails, email)
		if acc.ForwardMailboxes[email] == nil {
			provider, webHost := webmailProvider(email)
			acc.ForwardMailboxes[email] = &ForwardMailbox{Email: email, Provider: provider, WebHost: webHost}
		}
	}
	acc.AliasTotal = len(settings.Aliases)
	acc.AliasActive = 0
	for _, alias := range settings.Aliases {
		if alias.Active {
			acc.AliasActive++
		}
	}
	return m.save()
}

func (m *Manager) recordForwardingError(id string, cfg ForwardMailbox, validationErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return
	}
	if acc.ForwardMailboxes == nil {
		acc.ForwardMailboxes = make(map[string]*ForwardMailbox)
	}
	previous := acc.ForwardMailboxes[cfg.Email]
	if previous != nil && previous.Authorized {
		preserved := *previous
		preserved.LastError = truncate(validationErr.Error(), 300)
		acc.ForwardMailboxes[cfg.Email] = &preserved
		_ = m.save()
		return
	}
	cfg.Cookies = nil
	cfg.SessionID = ""
	cfg.Authorized = false
	cfg.LastError = truncate(validationErr.Error(), 300)
	acc.ForwardMailboxes[cfg.Email] = &cfg
	_ = m.save()
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func webmailProvider(email string) (string, string) {
	domain := ""
	if parts := strings.SplitN(normalizeEmail(email), "@", 2); len(parts) == 2 {
		domain = parts[1]
	}
	switch domain {
	case "163.com":
		return "netease_163", "mail.163.com"
	default:
		return "", ""
	}
}

func normalizeICloudHost(host string) (string, error) {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return "icloud.com", nil
	}
	if strings.Contains(host, "://") {
		u, err := url.Parse(host)
		if err != nil || u.Hostname() == "" {
			return "", fmt.Errorf("invalid iCloud host")
		}
		host = u.Hostname()
	}
	if host == "icloud.com" || strings.HasSuffix(host, ".icloud.com") {
		return "icloud.com", nil
	}
	if host == "icloud.com.cn" || strings.HasSuffix(host, ".icloud.com.cn") {
		return "icloud.com.cn", nil
	}
	return "", fmt.Errorf("unsupported iCloud host: %s", host)
}

// deriveICloudEmail 从账号身份推导 iCloud 邮箱地址(用于 IMAP 登录)。
//
// 规则:
//  1. primaryEmail 是 @icloud.com/@me.com/@mac.com → 直接用
//  2. appleId 是上述域名 → 直接用
//  3. appleId 是第三方邮箱(如 @qq.com) → 取 local part 拼 @icloud.com
func deriveICloudEmail(info *hme.AccountInfo) string {
	primary := strings.TrimSpace(info.PrimaryEmail)
	appleID := strings.TrimSpace(info.AppleID)

	if isICloudDomain(primary) {
		return primary
	}
	if isICloudDomain(appleID) {
		return appleID
	}
	if strings.Contains(appleID, "@") {
		local := strings.SplitN(appleID, "@", 2)[0]
		return local + "@icloud.com"
	}
	return firstNonEmpty(primary, appleID)
}

func isICloudDomain(email string) bool {
	return email != "" && (strings.Contains(email, "@icloud.com") ||
		strings.Contains(email, "@me.com") ||
		strings.Contains(email, "@mac.com"))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
