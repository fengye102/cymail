package mail

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/server"
)

// ---- 纯逻辑测试:游标 / 重叠 / 退避 / 分块 ----

func TestIdleOverlapStart(t *testing.T) {
	cases := []struct {
		cursor, margin, want uint32
	}{
		{0, 100, 1},
		{1, 100, 1},
		{50, 100, 1},
		{100, 100, 1},
		{101, 100, 1},
		{500, 100, 400},
		{1000, 200, 800},
		{300, 500, 1},
	}
	for _, tc := range cases {
		if got := idleOverlapStart(tc.cursor, tc.margin); got != tc.want {
			t.Errorf("idleOverlapStart(%d, %d) = %d, want %d", tc.cursor, tc.margin, got, tc.want)
		}
	}
}

func TestIdleChunkEnd(t *testing.T) {
	cases := []struct {
		from, uidNext, chunk, want uint32
	}{
		{1, 7, 100, 6},       // 单块:整区间 [1,7)
		{400, 600, 100, 499}, // 区间内切块
		{500, 600, 100, 599}, // 末块到区间末尾
		{1, 1, 100, 0},       // 空区间
	}
	for _, tc := range cases {
		if got := idleChunkEnd(tc.from, tc.uidNext, tc.chunk); got != tc.want {
			t.Errorf("idleChunkEnd(%d, %d, %d) = %d, want %d", tc.from, tc.uidNext, tc.chunk, got, tc.want)
		}
	}
}

func TestIdleNextBackoff(t *testing.T) {
	const base = time.Second
	const max = 30 * time.Second

	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	got := idleNextBackoff(0, base, max)
	for i, w := range want {
		if got != w {
			t.Fatalf("backoff step %d = %v, want %v", i, got, w)
		}
		got = idleNextBackoff(got, base, max)
	}
	// max 小于当前值时直接封顶
	if v := idleNextBackoff(time.Second, base, 500*time.Millisecond); v != 500*time.Millisecond {
		t.Fatalf("backoff cap = %v, want 500ms", v)
	}
}

func TestIdleNormalizeOptions(t *testing.T) {
	o := normalizeIdleOptions(IdleOptions{})
	if o.IdleTimeout != idleDefaultTimeout {
		t.Fatalf("default IdleTimeout = %v, want %v", o.IdleTimeout, idleDefaultTimeout)
	}
	if o.ReconnectMax != idleDefaultReconnectMax {
		t.Fatalf("default ReconnectMax = %v, want %v", o.ReconnectMax, idleDefaultReconnectMax)
	}
	o = normalizeIdleOptions(IdleOptions{IdleTimeout: 5 * time.Second, ReconnectMax: 2 * time.Second})
	if o.IdleTimeout != 5*time.Second || o.ReconnectMax != 2*time.Second {
		t.Fatalf("explicit options not preserved: %+v", o)
	}
}

func TestIdleActivityUpdate(t *testing.T) {
	if !idleActivityUpdate(&client.MailboxUpdate{Mailbox: &imap.MailboxStatus{Name: "INBOX"}}) {
		t.Error("MailboxUpdate(EXISTS/RECENT) should wake IDLE")
	}
	if !idleActivityUpdate(&client.ExpungeUpdate{SeqNum: 1}) {
		t.Error("ExpungeUpdate should wake IDLE")
	}
	if !idleActivityUpdate(&client.StatusUpdate{Status: &imap.StatusResp{Type: imap.StatusRespBye}}) {
		t.Error("StatusUpdate(BYE) should wake IDLE")
	}
	if idleActivityUpdate(&client.MessageUpdate{Message: &imap.Message{}}) {
		t.Error("MessageUpdate(纯 FETCH) should not wake IDLE")
	}
}

// ---- 假 IMAP 服务器:完整 IDLE 循环 ----

// testUpdaterBackend 包装 memory backend,支持向已连接客户端广播未请求更新。
type testUpdaterBackend struct {
	*memory.Backend
	updates chan backend.Update
}

func (b *testUpdaterBackend) Updates() <-chan backend.Update { return b.updates }

// startFakeIMAPServer 启动一个明文内存 IMAP 服务器(用户 username/password,
// INBOX 预置一封 UID=6 的邮件),返回 "host:port" 与可广播更新的 backend。
func startFakeIMAPServer(t *testing.T) (addr string, be *testUpdaterBackend, cleanup func()) {
	t.Helper()
	be = &testUpdaterBackend{Backend: memory.New(), updates: make(chan backend.Update, 16)}
	srv := server.New(be)
	srv.AllowInsecureAuth = true
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), be, func() {
		_ = srv.Close()
		_ = ln.Close()
	}
}

// newTestWatchClient 构造一个走明文拨号(测试注入)的 Client。
func newTestWatchClient(t *testing.T, addr string) *Client {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	c := NewIMAPClient(host, port, "username", "password")
	c.dialOverride = func(a string) (*client.Client, error) { return client.Dial(a) }
	return c
}

// pushExists 广播一封新邮件的 EXISTS/UIDNEXT 更新,模拟服务器主动通知。
func pushExists(be *testUpdaterBackend, messages, uidNext uint32) {
	be.updates <- &backend.MailboxUpdate{
		Update: backend.NewUpdate("username", "INBOX"),
		MailboxStatus: &imap.MailboxStatus{
			Name:     "INBOX",
			Messages: messages,
			UidNext:  uidNext,
			Items: map[imap.StatusItem]interface{}{
				imap.StatusMessages: nil,
				imap.StatusUidNext:  nil,
			},
		},
	}
}

// appendViaSecondConn 用第二个连接 APPEND 一封新邮件(UID 由服务器分配)。
func appendViaSecondConn(t *testing.T, addr string) {
	t.Helper()
	app, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial appender: %v", err)
	}
	defer func() { _ = app.Logout() }()
	if err := app.Login("username", "password"); err != nil {
		t.Fatalf("appender login: %v", err)
	}
	raw := "From: sender@example.com\r\n" +
		"To: username@example.com\r\n" +
		"Subject: hello\r\n" +
		"Date: Tue, 1 Jan 2019 10:00:00 +0000\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"hello world"
	if err := app.Append("INBOX", nil, time.Now(), strings.NewReader(raw)); err != nil {
		t.Fatalf("append: %v", err)
	}
}

type idleBatch struct {
	messages []Message
	nextUID  uint32
}

type idleRecorder struct {
	mu      sync.Mutex
	batches []idleBatch
}

func (r *idleRecorder) handler(messages []Message, nextUID uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]Message, len(messages))
	copy(cp, messages)
	r.batches = append(r.batches, idleBatch{messages: cp, nextUID: nextUID})
	return nil
}

func (r *idleRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.batches)
}

func (r *idleRecorder) at(i int) idleBatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.batches) {
		panic("idleRecorder.at: index out of range")
	}
	return r.batches[i]
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, desc string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", desc)
}

// startWatcher 启动 IdleWatchInbox,返回 ctx 的 cancel 与结果通道。
func startWatcher(c *Client, opts IdleOptions, rec *idleRecorder) (context.CancelFunc, <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.IdleWatchInbox(ctx, opts, rec.handler) }()
	return cancel, done
}

// TestIdleWatchInboxBackfillAndIncremental 覆盖完整循环:
// 首次连接重叠补拉 → IDLE → 新邮件通知(EXISTS 广播)立即唤醒 → 增量拉取回调 → 优雅退出。
func TestIdleWatchInboxBackfillAndIncremental(t *testing.T) {
	addr, be, cleanup := startFakeIMAPServer(t)
	defer cleanup()
	c := newTestWatchClient(t, addr)

	rec := &idleRecorder{}
	cancel, done := startWatcher(c, IdleOptions{
		StartUID:     5, // 预置邮件 UID=6,触发补拉 [1,7)
		IdleTimeout:  150 * time.Millisecond,
		ReconnectMax: 300 * time.Millisecond,
	}, rec)

	// 1) 补拉:应回调 UID=6,nextUID=7
	waitFor(t, 5*time.Second, func() bool { return rec.count() >= 1 }, "initial backfill callback")
	b := rec.at(0)
	if len(b.messages) != 1 || b.messages[0].ID != "6" {
		t.Fatalf("backfill batch = %+v, want one message with ID 6", b.messages)
	}
	if b.nextUID != 7 {
		t.Fatalf("backfill nextUID = %d, want 7", b.nextUID)
	}

	// 2) 新邮件到达(UID=7),广播 EXISTS 通知
	appendViaSecondConn(t, addr)
	pushExists(be, 2, 8)

	// 3) 增量回调:UID=7,nextUID=8
	waitFor(t, 5*time.Second, func() bool { return rec.count() >= 2 }, "incremental callback")
	b = rec.at(1)
	if len(b.messages) != 1 || b.messages[0].ID != "7" {
		t.Fatalf("incremental batch = %+v, want one message with ID 7", b.messages)
	}
	if b.nextUID != 8 {
		t.Fatalf("incremental nextUID = %d, want 8", b.nextUID)
	}

	// 4) ctx 取消:优雅退出,返回 nil
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("IdleWatchInbox returned error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("IdleWatchInbox did not exit after cancel")
	}
}

// TestIdleWatchInboxStartUIDZeroSkipsExistingMail 验证 StartUID=0 不回补历史,
// 只增量收新邮件。
func TestIdleWatchInboxStartUIDZeroSkipsExistingMail(t *testing.T) {
	addr, be, cleanup := startFakeIMAPServer(t)
	defer cleanup()
	c := newTestWatchClient(t, addr)

	rec := &idleRecorder{}
	cancel, done := startWatcher(c, IdleOptions{
		StartUID:     0, // 自动起点:不补拉预置的 UID=6
		IdleTimeout:  100 * time.Millisecond,
		ReconnectMax: 300 * time.Millisecond,
	}, rec)

	// 等 watcher 完成首个周期(至少一次 SELECT+IDLE),期间不应有任何回调
	time.Sleep(350 * time.Millisecond)
	if rec.count() != 0 {
		t.Fatalf("StartUID=0 must not backfill existing mail, got %d batches", rec.count())
	}

	// 新邮件到达 → 只回调新邮件 UID=7
	appendViaSecondConn(t, addr)
	pushExists(be, 2, 8)

	waitFor(t, 5*time.Second, func() bool { return rec.count() >= 1 }, "incremental callback")
	b := rec.at(0)
	if len(b.messages) != 1 || b.messages[0].ID != "7" {
		t.Fatalf("batch = %+v, want one message with ID 7", b.messages)
	}
	if b.nextUID != 8 {
		t.Fatalf("nextUID = %d, want 8", b.nextUID)
	}
	if rec.count() != 1 {
		t.Fatalf("got %d batches, want exactly 1", rec.count())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("IdleWatchInbox returned error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("IdleWatchInbox did not exit after cancel")
	}
}

// TestIdleWatchInboxReconnectsAfterServerDisconnect 覆盖断线重连:
// 服务器 BYE 断开 → 指数退避后自动重连 → 游标精确续传(不重放已处理邮件)。
func TestIdleWatchInboxReconnectsAfterServerDisconnect(t *testing.T) {
	addr, be, cleanup := startFakeIMAPServer(t)
	defer cleanup()
	c := newTestWatchClient(t, addr)

	rec := &idleRecorder{}
	cancel, done := startWatcher(c, IdleOptions{
		StartUID:     5,
		IdleTimeout:  150 * time.Millisecond,
		ReconnectMax: 300 * time.Millisecond,
	}, rec)

	// 1) 补拉 UID=6
	waitFor(t, 5*time.Second, func() bool { return rec.count() >= 1 }, "initial backfill callback")

	// 2) 新邮件 UID=7 → 增量回调
	appendViaSecondConn(t, addr)
	pushExists(be, 2, 8)
	waitFor(t, 5*time.Second, func() bool { return rec.count() >= 2 }, "incremental callback")

	// 3) 服务器 BYE 断开连接
	be.updates <- &backend.StatusUpdate{
		Update:     backend.NewUpdate("username", "INBOX"),
		StatusResp: &imap.StatusResp{Type: imap.StatusRespBye, Info: "test bye"},
	}

	// 4) 等 watcher 完成重连(300ms 退避 + 重连握手 + 首个周期)
	time.Sleep(800 * time.Millisecond)
	if rec.count() != 2 {
		t.Fatalf("reconnect must not replay already processed mail, got %d batches", rec.count())
	}

	// 5) 重连后再来新邮件 UID=8 → 继续增量回调(证明重连后循环恢复)
	appendViaSecondConn(t, addr)
	pushExists(be, 3, 9)
	waitFor(t, 5*time.Second, func() bool { return rec.count() >= 3 }, "post-reconnect incremental callback")
	b := rec.at(2)
	if len(b.messages) != 1 || b.messages[0].ID != "8" {
		t.Fatalf("post-reconnect batch = %+v, want one message with ID 8", b.messages)
	}
	if b.nextUID != 9 {
		t.Fatalf("post-reconnect nextUID = %d, want 9", b.nextUID)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("IdleWatchInbox returned error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("IdleWatchInbox did not exit after cancel")
	}
}

// TestIdleWatchInboxHandlerErrorTerminates 验证回调出错立即终止并返回该错误。
func TestIdleWatchInboxHandlerErrorTerminates(t *testing.T) {
	addr, _, cleanup := startFakeIMAPServer(t)
	defer cleanup()
	c := newTestWatchClient(t, addr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.IdleWatchInbox(ctx, IdleOptions{StartUID: 5, IdleTimeout: 150 * time.Millisecond, ReconnectMax: 300 * time.Millisecond},
			func(messages []Message, nextUID uint32) error {
				return &errTestAbort{}
			})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want handler error, got nil")
		}
		if _, ok := err.(*errTestAbort); !ok {
			t.Fatalf("want errTestAbort, got %T: %v", err, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("IdleWatchInbox did not terminate on handler error")
	}
}

type errTestAbort struct{}

func (e *errTestAbort) Error() string { return "test abort" }
