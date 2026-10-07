package account

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEncryptedPersistenceAndRedaction(t *testing.T) {
	dir := t.TempDir()
	dataFile := filepath.Join(dir, "accounts.json")
	plaintest := `{"accounts":{"acc_test":{"id":"acc_test","name":"test","real_email":"test@example.com","icloud_email":"test@icloud.com","cookies":{"session":"cookie-secret"},"host":"icloud.com","proxy":"http://user:proxy-secret@example.com:8080","app_password":"app-secret","status":"active"}}}`
	if err := os.WriteFile(dataFile, []byte(plaintest), 0600); err != nil {
		t.Fatal(err)
	}

	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	mgr, err := NewManagerWithKey(dir, key)
	if err != nil {
		t.Fatal(err)
	}

	accounts := mgr.ListAccounts()
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1", len(accounts))
	}
	public := accounts[0]
	if public.Cookies != nil || public.AppPassword != "" || public.Proxy != "" {
		t.Fatalf("public account contains reusable credentials: %#v", public)
	}

	if err := mgr.SaveCookies("acc_test", map[string]string{"session": "rotated-secret"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), encryptedDataPrefix) {
		t.Fatal("accounts.json was not encrypted")
	}
	if strings.Contains(string(raw), "rotated-secret") || strings.Contains(string(raw), "app-secret") {
		t.Fatal("encrypted file contains plaintext credentials")
	}

	reopened, err := NewManagerWithKey(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	private, ok := reopened.GetAccount("acc_test")
	if !ok {
		t.Fatal("reopened account missing")
	}
	if private.Cookies["session"] != "rotated-secret" || private.AppPassword != "app-secret" {
		t.Fatal("encrypted credentials did not round-trip")
	}

	private.Cookies["session"] = "mutated-copy"
	again, _ := reopened.GetAccount("acc_test")
	if again.Cookies["session"] != "rotated-secret" {
		t.Fatal("GetAccount returned a shared cookie map")
	}
}

func TestEncryptedFileRejectsWrongKey(t *testing.T) {
	dir := t.TempDir()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	mgr, err := NewManagerWithKey(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	mgr.accounts["acc_test"] = &Account{ID: "acc_test", Cookies: map[string]string{"x": "secret"}}
	err = mgr.save()
	mgr.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	wrongKey := base64.StdEncoding.EncodeToString([]byte("abcdef0123456789abcdef0123456789"))
	if _, err := NewManagerWithKey(dir, wrongKey); err == nil {
		t.Fatal("wrong encryption key was accepted")
	}
	if _, err := NewManager(dir); err == nil {
		t.Fatal("encrypted file was accepted without a key")
	}
}

func TestRejectsNonICloudHost(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AddAccount("test", "", "attacker.example", ""); err == nil {
		t.Fatal("non-iCloud host was accepted")
	}
}

func TestForwardMailboxCredentialsAreDeeplyRedacted(t *testing.T) {
	account := &Account{ForwardMailboxes: map[string]*ForwardMailbox{
		"receiver@example.com": {Email: "receiver@example.com", Cookies: map[string]string{"session": "web-secret"}, SessionID: "sid-secret", Authorized: true},
	}}
	public := account.Redacted()
	if public.ForwardMailboxes["receiver@example.com"].Cookies != nil || public.ForwardMailboxes["receiver@example.com"].SessionID != "" {
		t.Fatal("forwarding web session was exposed")
	}
	public.ForwardMailboxes["receiver@example.com"].Authorized = false
	if !account.ForwardMailboxes["receiver@example.com"].Authorized {
		t.Fatal("redacted account shares forwarding mailbox pointers")
	}
}

func TestAccountsStartupBackup(t *testing.T) {
	dir := t.TempDir()
	// 预置数据文件，模拟服务重启
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(`{"accounts":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(dir); err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("20060102")
	target := filepath.Join(dir, "backups", "accounts-"+today+".json")
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("startup daily backup missing: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("startup daily backup is empty")
	}
}

func TestAccountsDailyBackupIdempotentSameDay(t *testing.T) {
	dir := t.TempDir()
	mgr, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	mgr.accounts["acc_test"] = &Account{ID: "acc_test", Name: "test", Cookies: map[string]string{"x": "secret"}}
	err = mgr.save()
	mgr.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("20060102")
	target := filepath.Join(dir, "backups", "accounts-"+today+".json")
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("daily backup missing after save: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("daily backup is empty")
	}

	// 同一天再次保存：不产生第二份今日备份
	mgr.mu.Lock()
	mgr.accounts["acc_test"].Name = "renamed"
	err = mgr.save()
	mgr.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "backups", "accounts-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("backup files after second save = %d, want 1 (idempotent)", len(matches))
	}
}

func TestAccountsDailyBackupPrunesOldCopies(t *testing.T) {
	dir := t.TempDir()
	mgr, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 制造 12 份历史备份，日期严格早于今天
	backupsDir := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backupsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var names []string
	for i := 0; i < 12; i++ {
		day := now.AddDate(0, 0, -(i + 2))
		name := "accounts-" + day.Format("20060102") + ".json"
		if err := os.WriteFile(filepath.Join(backupsDir, name), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}

	// 触发一次保存：今日备份创建 + 保留最近 7 份清理
	mgr.mu.Lock()
	mgr.accounts["acc_test"] = &Account{ID: "acc_test"}
	err = mgr.save()
	mgr.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	matches, err := filepath.Glob(filepath.Join(backupsDir, "accounts-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 7 {
		t.Fatalf("backup files after prune = %d, want 7", len(matches))
	}
	// names 按插入序：names[0] 最近(today-2)，names[11] 最旧(today-13)；
	// 最旧的 6 份（names[6:]）必须被清理，today 与最近 6 份保留。
	for _, name := range names[6:] {
		if _, err := os.Stat(filepath.Join(backupsDir, name)); !os.IsNotExist(err) {
			t.Fatalf("old backup %s should have been pruned", name)
		}
	}
	for _, name := range names[:6] {
		if _, err := os.Stat(filepath.Join(backupsDir, name)); err != nil {
			t.Fatalf("recent backup %s should be kept: %v", name, err)
		}
	}
}

func TestPruneDailyBackupsIgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"accounts-20260101.json",     // 合法命名（数量少，不应被清）
		"accounts-note.txt",          // 后缀不匹配
		"other-20260101.json",        // 前缀不匹配
		"accounts-2026zz01.json",     // 日期部分非法
		"accounts-20260101.json.tmp", // 后缀不匹配
		"accounts-202601.json",       // 日期部分长度不足
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "accounts-20260102.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := pruneDailyBackups(dir, "accounts-", 7); err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("unrelated file %s was removed: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "accounts-20260102.json")); err != nil {
		t.Fatalf("unrelated directory was removed: %v", err)
	}
}
