// Command icloud-hme 启动 iCloud Hide My Email 多账号管理平台。
//
// 两个核心 HTTP 接口:
//
//	POST /api/create  — 创建隐私邮箱别名
//	GET  /api/inbox   — 读取邮件
//
// 用法:
//
//	./icloud-hme                    # 默认 127.0.0.1:8081
//	./icloud-hme -addr 127.0.0.1:9000
//	./icloud-hme -data ./data       # 指定数据目录
//	./icloud-hme -debug             # 调试模式
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/fulfillment"
	"icloud-hme/internal/server"
	"icloud-hme/internal/store"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8081", "HTTP 监听地址")
	dataDir := flag.String("data", "./data", "数据目录 (accounts.json 存放位置)")
	debug := flag.Bool("debug", false, "调试模式 (启用 Gin 调试日志)")
	apiKey := flag.String("api-key", firstNonEmptyEnv("CYMAIL_INTERNAL_API_KEY", "ICLOUD_HME_API_KEY"), "optional internal API key")
	apiKeyFile := flag.String("api-key-file", "", "file containing an internal API key; created when missing")
	adminAuthFile := flag.String("admin-auth-file", "", "administrator credential file (defaults to <data>/admin-auth.json)")
	dataKeyFile := flag.String("data-key-file", "", "file containing the base64 data encryption key")
	insecurePlaintextStorage := flag.Bool("insecure-plaintext-storage", false, "store credentials as plaintext")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file")
	tlsKey := flag.String("tls-key", "", "TLS private key file")
	allowInsecureHTTP := flag.Bool("allow-insecure-http", false, "allow plaintext HTTP on a non-loopback address")
	databaseURL := flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")
	publicBaseURL := flag.String("public-base-url", os.Getenv("PUBLIC_BASE_URL"), "public HTTPS base URL used in pickup links")
	collectInterval := flag.Duration("collect-interval", 30*time.Second, "mail collection interval; 0 disables automatic collection")
	memoryStore := flag.Bool("memory-store", false, "use a non-persistent in-memory store for local development")
	fileStorePath := flag.String("file-store", "", "local fulfillment JSON store path (defaults to <data>/fulfillment.json when PostgreSQL is not configured)")
	memorySeed := flag.Bool("memory-seed", false, "seed local in-memory store with a demo mailbox and messages")
	flag.Parse()

	loopbackOnly := isLoopbackAddress(*addr)
	publicURLValue, err := normalizePublicBaseURL(*publicBaseURL)
	if err != nil {
		log.Fatalf("PUBLIC_BASE_URL 配置错误: %v", err)
	}
	apiKeyValue := strings.TrimSpace(*apiKey)
	if apiKeyValue != "" && strings.TrimSpace(*apiKeyFile) != "" {
		log.Fatal("configure only one of -api-key or -api-key-file")
	}
	if strings.TrimSpace(*apiKeyFile) != "" {
		apiKeyPath, pathErr := filepath.Abs(strings.TrimSpace(*apiKeyFile))
		if pathErr != nil {
			log.Fatalf("resolve internal API key file: %v", pathErr)
		}
		apiKeyValue, err = loadOrCreateInternalAPIKey(apiKeyPath)
		if err != nil {
			log.Fatalf("load internal API key: %v", err)
		}
	}
	if apiKeyValue != "" && len(apiKeyValue) < 32 {
		log.Fatal("API key must be at least 32 characters")
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		log.Fatal("-tls-cert and -tls-key must be provided together")
	}
	if !loopbackOnly && *tlsCert == "" && !*allowInsecureHTTP {
		log.Fatal("non-loopback listeners require TLS; provide -tls-cert/-tls-key or explicitly use -allow-insecure-http")
	}
	if !loopbackOnly && *tlsCert == "" {
		log.Printf("WARNING: serving plaintext HTTP on a non-loopback address")
	}

	dataKey := strings.TrimSpace(os.Getenv("ICLOUD_HME_DATA_KEY"))
	if *dataKeyFile != "" {
		if dataKey != "" {
			log.Fatal("configure only one of ICLOUD_HME_DATA_KEY or -data-key-file")
		}
		rawKey, err := os.ReadFile(*dataKeyFile)
		if err != nil {
			log.Fatalf("read data encryption key: %v", err)
		}
		dataKey = strings.TrimSpace(string(rawKey))
	}
	if dataKey == "" && !*insecurePlaintextStorage {
		log.Fatal("data encryption key is required; set ICLOUD_HME_DATA_KEY, -data-key-file, or explicitly use -insecure-plaintext-storage")
	}
	if dataKey == "" {
		log.Printf("WARNING: credentials will be stored as plaintext")
	}

	log.Printf("iCloud Hide My Email 服务启动 addr=%s", *addr)

	abs, err := filepath.Abs(*dataDir)
	if err != nil {
		log.Fatalf("数据目录路径错误: %v", err)
	}
	adminAuthPath := strings.TrimSpace(*adminAuthFile)
	if adminAuthPath == "" {
		adminAuthPath = filepath.Join(abs, "admin-auth.json")
	} else if !filepath.IsAbs(adminAuthPath) {
		adminAuthPath, err = filepath.Abs(adminAuthPath)
		if err != nil {
			log.Fatalf("resolve admin auth file: %v", err)
		}
	}

	var mgr *account.Manager
	if dataKey != "" {
		mgr, err = account.NewManagerWithKey(abs, dataKey)
	} else {
		mgr, err = account.NewManager(abs)
	}
	if err != nil {
		log.Fatalf("初始化账号管理器失败: %v", err)
	}
	count := len(mgr.ListAccounts())
	log.Printf("账号加载完成 count=%d data_dir=%s", count, abs)

	if *memoryStore && (strings.TrimSpace(*databaseURL) != "" || strings.TrimSpace(*fileStorePath) != "") {
		log.Fatal("configure only one fulfillment store: -memory-store, -database-url, or -file-store")
	}
	if strings.TrimSpace(*databaseURL) != "" && strings.TrimSpace(*fileStorePath) != "" {
		log.Fatal("configure only one fulfillment store: -database-url or -file-store")
	}

	var fulfillmentService *fulfillment.Service
	if *memoryStore {
		memory := store.NewMemory()
		fulfillmentService = fulfillment.New(mgr, memory)
		log.Printf("WARNING: using non-persistent in-memory store")
		if *memorySeed {
			seedLocalDemo(memory, fulfillmentService, publicURLValue)
		}
	} else if strings.TrimSpace(*databaseURL) != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		pgStore, openErr := store.OpenPostgres(ctx, strings.TrimSpace(*databaseURL))
		if openErr != nil {
			cancel()
			log.Fatalf("连接 PostgreSQL 失败: %v", openErr)
		}
		if migrateErr := pgStore.Migrate(ctx); migrateErr != nil {
			cancel()
			pgStore.Close()
			log.Fatalf("初始化数据库失败: %v", migrateErr)
		}
		cancel()
		defer pgStore.Close()
		fulfillmentService = fulfillment.New(mgr, pgStore)
		log.Printf("库存与取件服务已启用")
	} else {
		path := strings.TrimSpace(*fileStorePath)
		if path == "" {
			path = filepath.Join(abs, "fulfillment.json")
		} else if !filepath.IsAbs(path) {
			path, err = filepath.Abs(path)
			if err != nil {
				log.Fatalf("resolve file store path: %v", err)
			}
		}
		fileStore, openErr := store.OpenFile(path)
		if openErr != nil {
			log.Fatalf("open local fulfillment store: %v", openErr)
		}
		if migrateErr := fileStore.Migrate(context.Background()); migrateErr != nil {
			log.Fatalf("initialize local fulfillment store: %v", migrateErr)
		}
		defer fileStore.Close()
		fulfillmentService = fulfillment.New(mgr, fileStore)
		log.Printf("local persistent fulfillment store enabled path=%s", path)
	}
	if fulfillmentService != nil && *collectInterval > 0 {
		go func() {
			ticker := time.NewTicker(*collectInterval)
			defer ticker.Stop()
			for range ticker.C {
				collectCtx, collectCancel := context.WithTimeout(context.Background(), 2*time.Minute)
				processed, failures, collectErr := fulfillmentService.Collect(collectCtx)
				collectCancel()
				if collectErr != nil {
					log.Printf("自动收件失败: %v", collectErr)
					continue
				}
				if processed > 0 || len(failures) > 0 {
					log.Printf("自动收件完成 processed=%d failures=%d", processed, len(failures))
				}
			}
		}()
	}

	srv, err := server.NewWithConfigE(mgr, server.Config{
		Debug:         *debug,
		APIKey:        apiKeyValue,
		AdminAuthFile: adminAuthPath,
		Fulfillment:   fulfillmentService,
		PublicBaseURL: publicURLValue,
		DataDir:       *dataDir,
	})
	if err != nil {
		log.Fatalf("初始化管理员登录失败: %v", err)
	}

	log.Printf("HTTP 服务就绪 addr=%s", *addr)
	var runErr error
	if *tlsCert != "" {
		runErr = srv.RunTLS(*addr, *tlsCert, *tlsKey)
	} else {
		runErr = srv.Run(*addr)
	}
	if runErr != nil {
		log.Fatalf("服务启动失败: %v", runErr)
	}
}

func seedLocalDemo(memory *store.Memory, service *fulfillment.Service, publicBaseURL string) {
	ctx := context.Background()
	mailbox := store.Mailbox{ID: "demo-mailbox", AccountID: "demo-account", Address: "aloe_26panics+gpte9y@icloud.com", Label: "本地演示", Status: "available", CreatedAt: time.Now().Add(-time.Hour)}
	if err := memory.UpsertMailboxes(ctx, []store.Mailbox{mailbox}); err != nil {
		log.Printf("seed demo mailbox: %v", err)
		return
	}
	order, key, err := service.AllocateWithPickupKey(ctx, "DEMO-ORDER", 24*time.Hour)
	if err != nil {
		log.Printf("seed demo order: %v", err)
		return
	}
	now := time.Now()
	messages := []store.Message{
		{MailboxID: order.MailboxID, AccountID: "demo-account", ProviderMessageID: "demo-1", Sender: "OpenAI <noreply@openai.com>", Recipient: order.MailboxAddress, Subject: "Your temporary ChatGPT login code", BodyText: "You can also enter this temporary code:\n\n753664\n\nThis code expires in 10 minutes.", OTPCode: "753664", ReceivedAt: now.Add(2 * time.Second)},
		{MailboxID: order.MailboxID, AccountID: "demo-account", ProviderMessageID: "demo-2", Sender: "OpenAI <noreply@openai.com>", Recipient: order.MailboxAddress, Subject: "New sign-in to your OpenAI account", BodyText: "We noticed a new sign-in to your account. If this was you, no action is required.", ReceivedAt: now.Add(time.Second)},
		{MailboxID: order.MailboxID, AccountID: "demo-account", ProviderMessageID: "demo-3", Sender: "ChatGPT <account@openai.com>", Recipient: order.MailboxAddress, Subject: "你的临时 ChatGPT 登录代码", BodyText: "验证码：266023\n\n请勿将此验证码分享给其他人。", OTPCode: "266023", ReceivedAt: now},
	}
	for _, message := range messages {
		if err := memory.SaveMessage(ctx, message); err != nil {
			log.Printf("seed demo message: %v", err)
		}
	}
	fragment := url.Values{}
	fragment.Set("email", order.MailboxAddress)
	fragment.Set("key", key)
	pickupURL := strings.TrimRight(publicBaseURL, "/") + "/pickup#" + fragment.Encode()
	log.Printf("DEMO PICKUP url=%s key=%s", pickupURL, key)
}

func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func loadOrCreateInternalAPIKey(path string) (string, error) {
	if raw, err := os.ReadFile(path); err == nil {
		value := strings.TrimSpace(string(raw))
		if len(value) < 32 {
			return "", errors.New("existing key is shorter than 32 characters")
		}
		return value, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	value := base64.RawURLEncoding.EncodeToString(random)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return loadOrCreateInternalAPIKey(path)
	}
	if err != nil {
		return "", err
	}
	if _, err = file.WriteString(value + "\n"); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return value, nil
}

func isLoopbackAddress(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func normalizePublicBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", errors.New("must be an absolute URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("must not contain credentials, query, or fragment")
	}
	if u.Scheme != "https" {
		hostIP := net.ParseIP(u.Hostname())
		isLocal := strings.EqualFold(u.Hostname(), "localhost") || (hostIP != nil && hostIP.IsLoopback())
		if u.Scheme != "http" || !isLocal {
			return "", errors.New("must use HTTPS except for loopback development")
		}
	}
	basePath := strings.TrimRight(u.EscapedPath(), "/")
	if basePath == "/" {
		basePath = ""
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+basePath, "/"), nil
}
