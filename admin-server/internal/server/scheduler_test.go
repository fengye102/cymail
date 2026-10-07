package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/hme"
)

// fakeCreateFn 可编程的假创建函数。
type fakeCreateFn struct {
	mu       sync.Mutex
	calls    map[string]int // accountID → 调用次数
	failWith error          // 非 nil 时所有账号返回该错误
	failOnce map[string]bool
	created  map[string]int
}

func newFakeCreateFn() *fakeCreateFn {
	return &fakeCreateFn{calls: make(map[string]int), failOnce: make(map[string]bool), created: make(map[string]int)}
}

func (f *fakeCreateFn) fn(accountID string) (*hme.CreateResult, string, error) {
	f.mu.Lock()
	f.calls[accountID]++
	fail := f.failWith
	if f.failOnce[accountID] {
		fail = errors.New("一次性失败")
		delete(f.failOnce, accountID)
	}
	f.mu.Unlock()
	if fail != nil {
		return nil, "", fail
	}
	f.mu.Lock()
	f.created[accountID]++
	f.mu.Unlock()
	return &hme.CreateResult{Email: "fake-" + accountID + "@privaterelay.icloud.com", Label: "fake", CreatedAt: time.Now().Format(time.RFC3339)}, "apple_account", nil
}

func (f *fakeCreateFn) callCount(accountID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[accountID]
}

func (f *fakeCreateFn) createdCount(accountID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created[accountID]
}

func newTestScheduler(fake *fakeCreateFn) *Scheduler {
	if fake == nil {
		fake = newFakeCreateFn()
	}
	return newScheduler(fake.fn, newAliasCreateRateLimiter())
}

func TestSchedulerRunsRoundsAndCounts(t *testing.T) {
	fake := newFakeCreateFn()
	sched := newTestScheduler(fake)
	job := sched.createJob("test", []string{"acc-1", "acc-2"}, time.Second, 2, defaultChannels())

	sched.runJob(context.Background(), job)

	if job.RoundsCompleted != 2 {
		t.Errorf("RoundsCompleted = %d, want 2", job.RoundsCompleted)
	}
	if job.CreatedCount != 4 {
		t.Errorf("CreatedCount = %d, want 4", job.CreatedCount)
	}
	if job.Status != "done" {
		t.Errorf("Status = %s, want done", job.Status)
	}
	if fake.callCount("acc-1") != 2 || fake.callCount("acc-2") != 2 {
		t.Errorf("call counts = %d/%d, want 2/2", fake.callCount("acc-1"), fake.callCount("acc-2"))
	}
	if len(job.Events) < 4 {
		t.Errorf("expected at least 4 events, got %d", len(job.Events))
	}
}

func TestSchedulerInfiniteRoundsStopsOnContext(t *testing.T) {
	fake := newFakeCreateFn()
	sched := newTestScheduler(fake)
	job := sched.createJob("test", []string{"acc-1"}, 10*time.Millisecond, 0, defaultChannels())

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	sched.runJob(ctx, job)

	if job.RoundsCompleted < 2 {
		t.Errorf("RoundsCompleted = %d, want >= 2 before cancel", job.RoundsCompleted)
	}
	if job.Status != "running" {
		t.Errorf("Status = %s, want running (cancelled, not done)", job.Status)
	}
}

func TestSchedulerSkipsFailingAccountButContinues(t *testing.T) {
	fake := newFakeCreateFn()
	fake.failWith = errors.New("旧接口会话失效")
	sched := newTestScheduler(fake)
	job := sched.createJob("test", []string{"acc-1", "acc-2"}, time.Millisecond, 2, defaultChannels())

	sched.runJob(context.Background(), job)

	if job.CreatedCount != 0 {
		t.Errorf("CreatedCount = %d, want 0 (all failed)", job.CreatedCount)
	}
	if job.RoundsCompleted != 2 {
		t.Errorf("RoundsCompleted = %d, want 2 (job continues despite failures)", job.RoundsCompleted)
	}
	if !strings.Contains(job.LastError, "会话失效") {
		t.Errorf("LastError = %q, want failure message recorded", job.LastError)
	}
	if fake.callCount("acc-1") != 2 {
		t.Errorf("acc-1 should be retried each round, calls = %d", fake.callCount("acc-1"))
	}
}

func TestSchedulerCapacitySkipsAccountForever(t *testing.T) {
	fake := newFakeCreateFn()
	fake.failWith = hme.ErrAliasLimitReached
	sched := newTestScheduler(fake)
	job := sched.createJob("test", []string{"acc-1", "acc-2"}, time.Millisecond, 3, defaultChannels())

	sched.runJob(context.Background(), job)

	if fake.callCount("acc-1") != 1 {
		t.Errorf("acc-1 should be attempted exactly once (capacity), got %d", fake.callCount("acc-1"))
	}
	if job.RoundsCompleted != 3 {
		t.Errorf("RoundsCompleted = %d, want 3", job.RoundsCompleted)
	}
}

func TestSchedulerRateLimitBlocksAccount(t *testing.T) {
	fake := newFakeCreateFn()
	fake.failWith = &hme.AliasRateLimitError{RetryAfter: time.Hour, Message: "rate limited"}
	sched := newTestScheduler(fake)
	job := sched.createJob("test", []string{"acc-1"}, time.Millisecond, 3, defaultChannels())

	sched.runJob(context.Background(), job)

	// 第一次尝试后账号被 block 1 小时, 后续轮次被 limiter 跳过
	if fake.callCount("acc-1") != 1 {
		t.Errorf("acc-1 calls = %d, want 1 (blocked after first rate limit)", fake.callCount("acc-1"))
	}
	if job.RoundsCompleted != 3 {
		t.Errorf("RoundsCompleted = %d, want 3", job.RoundsCompleted)
	}
	hasBlockEvent := false
	for _, event := range job.Events {
		if strings.Contains(event.Message, "限速") {
			hasBlockEvent = true
		}
	}
	if !hasBlockEvent {
		t.Error("expected a rate-limit event")
	}
}

func TestSchedulerPauseResumeDeleteStateMachine(t *testing.T) {
	fake := newFakeCreateFn()
	sched := newTestScheduler(fake)
	job := sched.createJob("test", []string{"acc-1"}, time.Millisecond, 0, defaultChannels())

	if job.Status != "running" {
		t.Errorf("new job status = %s, want running", job.Status)
	}
	if !sched.pauseJob(job.ID) {
		t.Fatal("pauseJob failed")
	}
	if job.Status != "paused" {
		t.Errorf("status after pause = %s, want paused", job.Status)
	}
	if !sched.resumeJob(job.ID, context.Background()) {
		t.Fatal("resumeJob failed")
	}
	if job.Status != "running" {
		t.Errorf("status after resume = %s, want running", job.Status)
	}
	if !sched.deleteJob(job.ID) {
		t.Fatal("deleteJob failed")
	}
	if _, exists := sched.getJob(job.ID); exists {
		t.Error("job still exists after delete")
	}
	if sched.pauseJob(job.ID) || sched.resumeJob(job.ID, context.Background()) {
		t.Error("operations on deleted job should fail")
	}
}

func TestSchedulerCreateJobValidation(t *testing.T) {
	srv := newTestServer(t, Config{APIKey: "test-key"})
	acc, err := srv.mgr.AddAccount("sched-test", "", "icloud.com", "")
	if err != nil {
		t.Fatal(err)
	}
	srv.ensureScheduler()
	router := schedulerTestRouter(srv, "test-key")

	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"interval too small", `{"account_ids":["` + acc.ID + `"],"interval_seconds":10}`, http.StatusBadRequest},
		{"missing accounts", `{"interval_seconds":300}`, http.StatusBadRequest},
		{"unknown account", `{"account_ids":["acc_missing"],"interval_seconds":300}`, http.StatusBadRequest},
		{"negative rounds", `{"account_ids":["` + acc.ID + `"],"interval_seconds":300,"rounds":-1}`, http.StatusBadRequest},
		{"all channels disabled", `{"account_ids":["` + acc.ID + `"],"interval_seconds":300,"channels":{"apple_account":false,"icloud_web":false}}`, http.StatusBadRequest},
		{"valid", `{"name":"夜间任务","account_ids":["` + acc.ID + `"],"interval_seconds":300,"rounds":0}`, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/scheduler/jobs", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer test-key")
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)
			if resp.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", resp.Code, tc.status, resp.Body.String())
			}
		})
	}

	// 有效任务会真实启动 goroutine (默认 createFn), 立即清理避免后台泄漏
	sched := srv.ensureScheduler()
	sched.stopAll()
}

func TestSchedulerJobEndpoints(t *testing.T) {
	srv := newTestServer(t, Config{APIKey: "test-key"})
	acc, err := srv.mgr.AddAccount("sched-e2e", "", "icloud.com", "")
	if err != nil {
		t.Fatal(err)
	}
	sched := srv.ensureScheduler()
	// 注入 fake, 避免真实网络
	sched.createFn = newFakeCreateFn().fn
	sched.limiter = nil
	router := schedulerTestRouter(srv, "test-key")

	create := httptest.NewRequest(http.MethodPost, "/api/scheduler/jobs", strings.NewReader(`{"name":"e2e","account_ids":["`+acc.ID+`"],"interval_seconds":30,"rounds":1}`))
	create.Header.Set("Authorization", "Bearer test-key")
	create.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	router.ServeHTTP(created, create)
	if created.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", created.Code, created.Body.String())
	}
	var payload struct {
		Data struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	machineDecodeData(t, created, &payload.Data)
	if payload.Data.Status != "running" {
		t.Errorf("job status = %s, want running", payload.Data.Status)
	}

	list := httptest.NewRequest(http.MethodGet, "/api/scheduler/jobs", nil)
	list.Header.Set("Authorization", "Bearer test-key")
	listResp := httptest.NewRecorder()
	router.ServeHTTP(listResp, list)
	if listResp.Code != http.StatusOK {
		t.Fatalf("list status = %d", listResp.Code)
	}
	if !strings.Contains(listResp.Body.String(), payload.Data.ID) {
		t.Error("job id not in list response")
	}

	pause := httptest.NewRequest(http.MethodPost, "/api/scheduler/jobs/"+payload.Data.ID+"/pause", nil)
	pause.Header.Set("Authorization", "Bearer test-key")
	pauseResp := httptest.NewRecorder()
	router.ServeHTTP(pauseResp, pause)
	if pauseResp.Code != http.StatusOK {
		t.Fatalf("pause status = %d", pauseResp.Code)
	}

	del := httptest.NewRequest(http.MethodDelete, "/api/scheduler/jobs/"+payload.Data.ID, nil)
	del.Header.Set("Authorization", "Bearer test-key")
	delResp := httptest.NewRecorder()
	router.ServeHTTP(delResp, del)
	if delResp.Code != http.StatusOK {
		t.Fatalf("delete status = %d", delResp.Code)
	}
}

// schedulerTestRouter 复刻 register() 中 /api 组的中间件接线, 挂载调度器路由。
func schedulerTestRouter(srv *Server, apiKey string) http.Handler {
	engine := gin.New()
	api := engine.Group("/api")
	api.Use(srv.secureHeaders(), srv.limitRequestBody(), srv.authenticate())
	srv.registerSchedulerRoutes(api)
	return engine
}

func defaultChannels() map[SchedulerChannel]bool {
	return map[SchedulerChannel]bool{SchedulerChannelAppleAccount: true, SchedulerChannelICloudWeb: true}
}
