// Package server - 服务端定时创建调度器
//
// 不依赖浏览器页面, 在服务端按配置轮次批量创建隐藏邮箱。
// 创建逻辑复用 createAliasPreferAppleAccount (新接口优先, 限速/会话失效回退旧接口),
// 并尊重 aliasCreateRateLimiter 的自定义间隔与 Apple 临时封禁。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"icloud-hme/internal/hme"
)

// SchedulerChannel 是创建渠道 (当前为元数据, 创建行为统一走双接口自动回退)。
type SchedulerChannel string

const (
	SchedulerChannelAppleAccount SchedulerChannel = "apple_account"
	SchedulerChannelICloudWeb    SchedulerChannel = "icloud_web"
)

// SchedulerJobEvent 记录调度任务的单条事件。
type SchedulerJobEvent struct {
	At      time.Time `json:"at"`
	Message string    `json:"message"`
}

// schedulerMinInterval 轮次最小间隔。
const schedulerMinInterval = 30 * time.Second

// schedulerMaxEvents 保留的最大事件条数 (先进先出)。
const schedulerMaxEvents = 100

// SchedulerJob 是一个定时创建任务。
type SchedulerJob struct {
	mu sync.Mutex

	ID              string                    `json:"id"`
	Name            string                    `json:"name"`
	AccountIDs      []string                  `json:"account_ids"`
	Interval        time.Duration             `json:"-"`
	IntervalSeconds int                       `json:"interval_seconds"`
	Rounds          int                       `json:"rounds"` // 0 = 无限
	ChannelsEnabled map[SchedulerChannel]bool `json:"channels"`
	Status          string                    `json:"status"` // running | paused | done | error
	CreatedAt       time.Time                 `json:"created_at"`
	LastRoundAt     time.Time                 `json:"last_round_at,omitempty"`
	RoundsCompleted int                       `json:"rounds_completed"`
	CreatedCount    int                       `json:"created_count"`
	LastError       string                    `json:"last_error,omitempty"`
	Events          []SchedulerJobEvent       `json:"events"`

	// skippedAccounts 记录总容量已满的账号, 后续轮次不再尝试。
	// 注意: 字段不导出, encoding/json 不会序列化它, 不能直接加 json 标签 (go vet 会报错);
	// 磁盘持久化经由 persistedJob.SkippedAccounts 完成 (见 toPersisted/persistedToJob)。
	skippedAccounts map[string]bool
	// cancel 用于暂停/删除时中断任务循环。
	cancel context.CancelFunc
}

func (j *SchedulerJob) lock()   { j.mu.Lock() }
func (j *SchedulerJob) unlock() { j.mu.Unlock() }
func (j *SchedulerJob) isDone() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Status == "done" || j.Status == "error"
}

func (j *SchedulerJob) addEvent(message string) {
	now := time.Now()
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Events = append(j.Events, SchedulerJobEvent{At: now, Message: message})
	if len(j.Events) > schedulerMaxEvents {
		j.Events = j.Events[len(j.Events)-schedulerMaxEvents:]
	}
}

func (j *SchedulerJob) setStatus(status string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Status = status
}

func (j *SchedulerJob) markRoundCompleted(now time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.RoundsCompleted++
	j.LastRoundAt = now
}

func (j *SchedulerJob) incrementCreated() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.CreatedCount++
}

func (j *SchedulerJob) setLastError(message string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.LastError = message
}

func (j *SchedulerJob) isSkipped(accountID string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.skippedAccounts[accountID]
}

func (j *SchedulerJob) markSkipped(accountID string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.skippedAccounts[accountID] = true
}

// roundsRemaining 返回是否还有轮次需要执行。
func (j *SchedulerJob) roundsRemaining() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Rounds <= 0 {
		return true
	}
	return j.RoundsCompleted < j.Rounds
}

// Scheduler 管理多个定时创建任务, 线程安全。
type Scheduler struct {
	mu          sync.Mutex
	jobs        map[string]*SchedulerJob
	cancels     map[string]context.CancelFunc
	createFn    func(accountID string) (*hme.CreateResult, string, error)
	limiter     *aliasCreateRateLimiter // 可为 nil (测试)
	persistPath string                  // 空 = 不持久化 (默认行为)
	persistMu   sync.Mutex              // 串行化写盘, 避免并发 rename 竞争
}

// newScheduler 创建调度器。
//
// createFn 返回 (创建结果, 使用的接口, 错误); 错误分类与现有创建流程一致
// (ErrAliasRateLimited / ErrAliasLimitReached / 其他)。
func newScheduler(createFn func(accountID string) (*hme.CreateResult, string, error), limiter *aliasCreateRateLimiter) *Scheduler {
	if createFn == nil {
		createFn = func(accountID string) (*hme.CreateResult, string, error) {
			return nil, "", errors.New("scheduler create function not configured")
		}
	}
	return &Scheduler{
		jobs:     make(map[string]*SchedulerJob),
		cancels:  make(map[string]context.CancelFunc),
		createFn: createFn,
		limiter:  limiter,
	}
}

// createJob 创建任务 (仅登记, 不启动; 由 startJob 显式启动)。
func (s *Scheduler) createJob(name string, accountIDs []string, interval time.Duration, rounds int, channels map[SchedulerChannel]bool) *SchedulerJob {
	job := &SchedulerJob{
		ID:              "sched_" + uuid.NewString()[:8],
		Name:            name,
		AccountIDs:      append([]string(nil), accountIDs...),
		Interval:        interval,
		IntervalSeconds: int(interval.Seconds()),
		Rounds:          rounds,
		ChannelsEnabled: channels,
		Status:          "running",
		CreatedAt:       time.Now(),
		Events:          []SchedulerJobEvent{{At: time.Now(), Message: "任务已创建"}},
		skippedAccounts: make(map[string]bool),
	}
	s.mu.Lock()
	s.jobs[job.ID] = job
	s.mu.Unlock()
	s.persistAndLog()
	return job
}

// startJob 启动任务 goroutine (若尚未运行)。
func (s *Scheduler) startJob(job *SchedulerJob, rootCtx context.Context) {
	ctx, cancel := context.WithCancel(rootCtx)
	s.mu.Lock()
	if job.cancel != nil {
		s.mu.Unlock()
		cancel()
		return
	}
	job.cancel = cancel
	s.cancels[job.ID] = cancel
	s.mu.Unlock()
	go func() {
		s.runJob(ctx, job)
		s.mu.Lock()
		if job.cancel != nil {
			job.cancel = nil
		}
		delete(s.cancels, job.ID)
		s.mu.Unlock()
	}()
}

// runJob 是任务的轮次循环 (同步可测)。
func (s *Scheduler) runJob(ctx context.Context, job *SchedulerJob) {
	if job.Status == "paused" {
		return
	}
	for job.roundsRemaining() {
		if ctx.Err() != nil {
			return
		}
		s.runOneRound(ctx, job)
		if !job.roundsRemaining() {
			break
		}
		timer := time.NewTimer(job.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	job.setStatus("done")
	job.addEvent("任务完成")
	s.persistAndLog()
}

// runOneRound 对任务内所有账号执行一轮创建。
func (s *Scheduler) runOneRound(ctx context.Context, job *SchedulerJob) {
	now := time.Now()
	for _, accountID := range job.AccountIDs {
		if ctx.Err() != nil {
			return
		}
		if job.isSkipped(accountID) {
			continue
		}
		if s.limiter != nil {
			if retryAfter := s.limiter.retryAfter(accountID, now, job.Interval); retryAfter > 0 {
				continue
			}
		}
		result, iface, err := s.createFn(accountID)
		switch {
		case err == nil:
			job.incrementCreated()
			job.addEvent("创建成功: " + result.Email + " (接口: " + iface + ")")
			if s.limiter != nil {
				s.limiter.recordSuccess(accountID, now)
			}
		case errors.Is(err, hme.ErrAliasLimitReached):
			job.markSkipped(accountID)
			job.addEvent("账号 " + accountID + " 总容量已满, 后续轮次不再尝试: " + err.Error())
		case errors.Is(err, hme.ErrAliasRateLimited):
			retryAfter := hme.AliasRetryAfter(err)
			if retryAfter <= 0 {
				retryAfter = time.Hour
			}
			if s.limiter != nil {
				s.limiter.block(accountID, now, retryAfter)
			}
			job.addEvent("账号 " + accountID + " 被 Apple 限速, 建议 " + retryAfter.String() + " 后重试")
		default:
			job.setLastError(err.Error())
			job.addEvent("账号 " + accountID + " 创建失败: " + err.Error())
		}
	}
	job.markRoundCompleted(time.Now())
	s.persistAndLog()
}

// pauseJob 暂停任务 (中断当前等待, 任务状态置为 paused)。
func (s *Scheduler) pauseJob(id string) bool {
	s.mu.Lock()
	job, ok := s.jobs[id]
	cancel := s.cancels[id]
	s.mu.Unlock()
	if !ok {
		return false
	}
	job.setStatus("paused")
	if cancel != nil {
		cancel()
	}
	job.addEvent("任务已暂停")
	s.persistAndLog()
	return true
}

// resumeJob 恢复任务。
func (s *Scheduler) resumeJob(id string, rootCtx context.Context) bool {
	s.mu.Lock()
	job, ok := s.jobs[id]
	s.mu.Unlock()
	if !ok {
		return false
	}
	if job.isDone() {
		return false
	}
	job.setStatus("running")
	job.addEvent("任务已恢复")
	s.persistAndLog()
	s.startJob(job, rootCtx)
	return true
}

// deleteJob 删除任务 (中断运行并移除)。
func (s *Scheduler) deleteJob(id string) bool {
	s.mu.Lock()
	job, ok := s.jobs[id]
	cancel := s.cancels[id]
	if ok {
		delete(s.jobs, id)
		delete(s.cancels, id)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	if cancel != nil {
		cancel()
	}
	job.addEvent("任务已删除")
	s.persistAndLog()
	return true
}

func (s *Scheduler) getJob(id string) (*SchedulerJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	return job, ok
}

func (s *Scheduler) listJobs() []*SchedulerJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*SchedulerJob, 0, len(s.jobs))
	for _, job := range s.jobs {
		out = append(out, job)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// stopAll 取消所有任务 (服务关闭时调用)。
func (s *Scheduler) stopAll() {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.cancels))
	for _, cancel := range s.cancels {
		cancels = append(cancels, cancel)
	}
	s.cancels = make(map[string]context.CancelFunc)
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// ---- 任务持久化 (重启恢复) ----

// persistedSchedulerState 是持久化状态文件的最外层结构。
type persistedSchedulerState struct {
	Version int             `json:"version"`
	SavedAt time.Time       `json:"saved_at"`
	Jobs    []*persistedJob `json:"jobs"`
}

// persistedJob 是 SchedulerJob 的磁盘表示, 只包含可序列化字段,
// 不包含运行时内部字段 (mutex/Interval/cancel/goroutine)。
type persistedJob struct {
	ID              string                    `json:"id"`
	Name            string                    `json:"name"`
	AccountIDs      []string                  `json:"account_ids"`
	IntervalSeconds int                       `json:"interval_seconds"`
	Rounds          int                       `json:"rounds"`
	ChannelsEnabled map[SchedulerChannel]bool `json:"channels"`
	Status          string                    `json:"status"` // running | paused | done | error
	CreatedAt       time.Time                 `json:"created_at"`
	LastRoundAt     time.Time                 `json:"last_round_at,omitempty"`
	RoundsCompleted int                       `json:"rounds_completed"`
	CreatedCount    int                       `json:"created_count"`
	LastError       string                    `json:"last_error,omitempty"`
	Events          []SchedulerJobEvent       `json:"events"`
	SkippedAccounts map[string]bool           `json:"skipped_accounts,omitempty"`
}

// SetPersistPath 设置持久化路径 (幂等; path 为空表示关闭持久化)。
// 设置后立即把当前 jobs 写盘一次; 写盘失败仅记录日志, 不影响主流程。
func (s *Scheduler) SetPersistPath(path string) {
	s.mu.Lock()
	if s.persistPath == path {
		s.mu.Unlock()
		return
	}
	s.persistPath = path
	s.mu.Unlock()
	if path != "" {
		s.persistAndLog()
	}
}

// persist 把当前全部 jobs 序列化写入 persistPath (临时文件 + rename 原子写)。
// 未设置 persistPath 时为 no-op。
func (s *Scheduler) persist() error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.Lock()
	path := s.persistPath
	jobs := make([]*SchedulerJob, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, job)
	}
	s.mu.Unlock()
	if path == "" {
		return nil
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.Before(jobs[j].CreatedAt) })
	state := persistedSchedulerState{
		Version: 1,
		SavedAt: time.Now(),
		Jobs:    make([]*persistedJob, 0, len(jobs)),
	}
	for _, job := range jobs {
		state.Jobs = append(state.Jobs, job.toPersisted())
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal scheduler jobs: %w", err)
	}
	return atomicWriteFile(path, data)
}

// persistAndLog 写盘, 失败仅记录日志, 不影响主流程。
func (s *Scheduler) persistAndLog() {
	if err := s.persist(); err != nil {
		log.Printf("[scheduler] 任务状态写盘失败 (%s): %v", s.persistPath, err)
	}
}

// toPersisted 在任务锁保护下深拷贝字段为可持久化表示。
func (j *SchedulerJob) toPersisted() *persistedJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	p := &persistedJob{
		ID:              j.ID,
		Name:            j.Name,
		AccountIDs:      append([]string(nil), j.AccountIDs...),
		IntervalSeconds: j.IntervalSeconds,
		Rounds:          j.Rounds,
		Status:          j.Status,
		CreatedAt:       j.CreatedAt,
		LastRoundAt:     j.LastRoundAt,
		RoundsCompleted: j.RoundsCompleted,
		CreatedCount:    j.CreatedCount,
		LastError:       j.LastError,
		Events:          append([]SchedulerJobEvent(nil), j.Events...),
	}
	if len(j.ChannelsEnabled) > 0 {
		p.ChannelsEnabled = make(map[SchedulerChannel]bool, len(j.ChannelsEnabled))
		for k, v := range j.ChannelsEnabled {
			p.ChannelsEnabled[k] = v
		}
	}
	if len(j.skippedAccounts) > 0 {
		p.SkippedAccounts = make(map[string]bool, len(j.skippedAccounts))
		for k, v := range j.skippedAccounts {
			p.SkippedAccounts[k] = v
		}
	}
	return p
}

// persistedToJob 把磁盘表示还原为 SchedulerJob (不含 cancel/goroutine)。
// 未知状态按 paused 恢复, 避免恢复后意外自动运行。
func persistedToJob(p *persistedJob) *SchedulerJob {
	job := &SchedulerJob{
		ID:              p.ID,
		Name:            p.Name,
		AccountIDs:      append([]string(nil), p.AccountIDs...),
		Interval:        time.Duration(p.IntervalSeconds) * time.Second,
		IntervalSeconds: p.IntervalSeconds,
		Rounds:          p.Rounds,
		Status:          p.Status,
		CreatedAt:       p.CreatedAt,
		LastRoundAt:     p.LastRoundAt,
		RoundsCompleted: p.RoundsCompleted,
		CreatedCount:    p.CreatedCount,
		LastError:       p.LastError,
		Events:          append([]SchedulerJobEvent(nil), p.Events...),
		skippedAccounts: make(map[string]bool),
	}
	if len(p.ChannelsEnabled) > 0 {
		job.ChannelsEnabled = make(map[SchedulerChannel]bool, len(p.ChannelsEnabled))
		for k, v := range p.ChannelsEnabled {
			job.ChannelsEnabled[k] = v
		}
	}
	for k, v := range p.SkippedAccounts {
		job.skippedAccounts[k] = v
	}
	switch job.Status {
	case "running", "paused", "done", "error":
	case "":
		job.Status = "paused"
	default:
		log.Printf("[scheduler] 持久化任务 %s 状态 %q 非法, 按 paused 恢复", job.ID, job.Status)
		job.Status = "paused"
	}
	return job
}

// LoadJobs 从持久化文件加载任务定义 (不含 cancel/goroutine, 不会启动任何任务)。
// 文件不存在时返回空列表 (不算错误); 文件整体损坏返回错误;
// 单个损坏条目 (缺 ID 等) 跳过并记录日志。
func LoadJobs(path string) ([]*SchedulerJob, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var state persistedSchedulerState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("解析调度器持久化文件 %s 失败: %w", path, err)
	}
	jobs := make([]*SchedulerJob, 0, len(state.Jobs))
	for _, p := range state.Jobs {
		if p == nil || p.ID == "" {
			log.Printf("[scheduler] 跳过损坏的持久化任务条目 (文件 %s)", path)
			continue
		}
		jobs = append(jobs, persistedToJob(p))
	}
	return jobs, nil
}

// RestoreAndStartPersisted 从 path 恢复持久化任务并启动其中 status==running 的任务。
// 若 Scheduler 尚未设置持久化路径则设置之 (此后所有变更都会写回该文件);
// 文件中与现有 jobs ID 冲突的条目跳过并记录日志;
// 恢复的 running 任务立即 startJob 继续运行 (skippedAccounts 已从 JSON 还原)。
// 返回值为实际启动 (原 status==running 且无 ID 冲突) 的任务数。
func (s *Scheduler) RestoreAndStartPersisted(path string, rootCtx context.Context) (int, error) {
	s.mu.Lock()
	if s.persistPath == "" {
		s.persistPath = path
	}
	s.mu.Unlock()
	loaded, err := LoadJobs(path)
	if err != nil {
		return 0, err
	}
	started := 0
	for _, job := range loaded {
		s.mu.Lock()
		_, exists := s.jobs[job.ID]
		if !exists {
			s.jobs[job.ID] = job
		}
		s.mu.Unlock()
		if exists {
			log.Printf("[scheduler] 跳过持久化任务 %s (ID 已存在)", job.ID)
			continue
		}
		if job.Status == "running" {
			s.startJob(job, rootCtx)
			started++
		}
	}
	s.persistAndLog()
	return started, nil
}

// atomicWriteFile 以临时文件 + rename 方式原子写入, 避免写一半留下损坏文件。
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".scheduler-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // rename 成功后为 no-op
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// ---- Server 集成 ----

// schedulerDefaultCreateFn 是调度任务的默认创建函数:
// 与手动创建一致的标签规则, 走双接口自动回退。
func (s *Server) schedulerDefaultCreateFn(accountID string) (*hme.CreateResult, string, error) {
	client, err := s.mgr.HMEClient(accountID, false)
	if err != nil {
		return nil, "", err
	}
	label := "Sched-" + time.Now().Format("0102-150405")
	return s.createAliasPreferAppleAccount(accountID, client, label)
}

// ensureScheduler 懒初始化调度器。
func (s *Server) ensureScheduler() *Scheduler {
	if s.sched == nil {
		s.sched = newScheduler(s.schedulerDefaultCreateFn, s.aliasCreateLimiter)
	}
	return s.sched
}

// registerSchedulerRoutes 注册调度器管理 API。
func (s *Server) registerSchedulerRoutes(r gin.IRouter) {
	sched := s.ensureScheduler()
	group := r.Group("/scheduler")
	group.POST("/jobs", func(c *gin.Context) { s.createSchedulerJob(c, sched) })
	group.GET("/jobs", func(c *gin.Context) { s.listSchedulerJobs(c, sched) })
	group.GET("/jobs/:id", func(c *gin.Context) { s.getSchedulerJob(c, sched) })
	group.POST("/jobs/:id/pause", func(c *gin.Context) { s.pauseSchedulerJob(c, sched) })
	group.POST("/jobs/:id/resume", func(c *gin.Context) { s.resumeSchedulerJob(c, sched) })
	group.DELETE("/jobs/:id", func(c *gin.Context) { s.deleteSchedulerJob(c, sched) })
}

type createSchedulerJobReq struct {
	Name            string                    `json:"name"`
	AccountIDs      []string                  `json:"account_ids" binding:"required"`
	IntervalSeconds int                       `json:"interval_seconds" binding:"required"`
	Rounds          int                       `json:"rounds"`
	Channels        map[SchedulerChannel]bool `json:"channels"`
}

func (s *Server) createSchedulerJob(c *gin.Context, sched *Scheduler) {
	var req createSchedulerJobReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: account_ids, interval_seconds 必填 — "+err.Error())
		return
	}
	if req.IntervalSeconds < int(schedulerMinInterval.Seconds()) {
		fail(c, http.StatusBadRequest, "interval_seconds 必须 >= 30")
		return
	}
	if req.Rounds < 0 {
		fail(c, http.StatusBadRequest, "rounds 不能为负数")
		return
	}
	if len(req.AccountIDs) == 0 {
		fail(c, http.StatusBadRequest, "account_ids 不能为空")
		return
	}
	unique := make(map[string]bool, len(req.AccountIDs))
	for _, id := range req.AccountIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := s.mgr.GetAccount(id); !ok {
			fail(c, http.StatusBadRequest, "账号不存在: "+id)
			return
		}
		unique[id] = true
	}
	if len(unique) == 0 {
		fail(c, http.StatusBadRequest, "account_ids 不能为空")
		return
	}
	channels := req.Channels
	if len(channels) == 0 {
		channels = map[SchedulerChannel]bool{SchedulerChannelAppleAccount: true, SchedulerChannelICloudWeb: true}
	}
	enabled := false
	for _, value := range channels {
		if value {
			enabled = true
			break
		}
	}
	if !enabled {
		fail(c, http.StatusBadRequest, "channels 至少启用一个")
		return
	}
	accountIDs := make([]string, 0, len(unique))
	for id := range unique {
		accountIDs = append(accountIDs, id)
	}
	job := sched.createJob(strings.TrimSpace(req.Name), accountIDs, time.Duration(req.IntervalSeconds)*time.Second, req.Rounds, channels)
	sched.startJob(job, s.schedRootCtx)
	ok(c, job)
}

func (s *Server) listSchedulerJobs(c *gin.Context, sched *Scheduler) {
	ok(c, sched.listJobs())
}

func (s *Server) getSchedulerJob(c *gin.Context, sched *Scheduler) {
	job, exists := sched.getJob(c.Param("id"))
	if !exists {
		fail(c, http.StatusNotFound, "调度任务不存在")
		return
	}
	ok(c, job)
}

func (s *Server) pauseSchedulerJob(c *gin.Context, sched *Scheduler) {
	if !sched.pauseJob(c.Param("id")) {
		fail(c, http.StatusNotFound, "调度任务不存在")
		return
	}
	ok(c, gin.H{"id": c.Param("id"), "status": "paused"})
}

func (s *Server) resumeSchedulerJob(c *gin.Context, sched *Scheduler) {
	if !sched.resumeJob(c.Param("id"), s.schedRootCtx) {
		job, exists := sched.getJob(c.Param("id"))
		if !exists {
			fail(c, http.StatusNotFound, "调度任务不存在")
			return
		}
		if job.isDone() {
			fail(c, http.StatusConflict, "任务已结束, 无法恢复")
			return
		}
	}
	ok(c, gin.H{"id": c.Param("id"), "status": "running"})
}

func (s *Server) deleteSchedulerJob(c *gin.Context, sched *Scheduler) {
	if !sched.deleteJob(c.Param("id")) {
		fail(c, http.StatusNotFound, "调度任务不存在")
		return
	}
	ok(c, gin.H{"id": c.Param("id"), "status": "deleted"})
}
