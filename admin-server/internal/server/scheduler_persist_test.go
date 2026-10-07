package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSchedulerPersistRestoreRoundTrip 验证 persist → LoadJobs 往返字段一致,
// 覆盖 running/paused/done 状态、计数与 skippedAccounts。
func TestSchedulerPersistRestoreRoundTrip(t *testing.T) {
	fake := newFakeCreateFn()
	sched := newTestScheduler(fake)
	job := sched.createJob("夜间任务", []string{"acc-1", "acc-2"}, 90*time.Second, 5, defaultChannels())

	// 跑一轮制造计数, 并模拟一个账号容量已满被跳过
	sched.runOneRound(context.Background(), job)
	job.markSkipped("acc-2")
	if !sched.pauseJob(job.ID) {
		t.Fatal("pauseJob failed")
	}

	// 额外造一个运行到 done 的任务
	doneJob := sched.createJob("已完成", []string{"acc-3"}, time.Second, 1, defaultChannels())
	sched.runJob(context.Background(), doneJob)
	if doneJob.Status != "done" {
		t.Fatalf("done job status = %s, want done", doneJob.Status)
	}

	path := filepath.Join(t.TempDir(), "scheduler-jobs.json")
	sched.SetPersistPath(path)
	sched.SetPersistPath(path) // 幂等: 重复设置不报错
	if err := sched.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("persist file not written: %v", err)
	}

	loaded, err := LoadJobs(path)
	if err != nil {
		t.Fatalf("LoadJobs: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("restored %d jobs, want 2", len(loaded))
	}

	var restored *SchedulerJob
	for _, j := range loaded {
		if j.ID == job.ID {
			restored = j
		}
	}
	if restored == nil {
		t.Fatal("job not restored")
	}
	if restored.ID != job.ID || restored.Name != job.Name {
		t.Errorf("id/name mismatch: %q/%q vs %q/%q", restored.ID, restored.Name, job.ID, job.Name)
	}
	if len(restored.AccountIDs) != 2 || restored.AccountIDs[0] != "acc-1" || restored.AccountIDs[1] != "acc-2" {
		t.Errorf("account_ids mismatch: %v", restored.AccountIDs)
	}
	if restored.IntervalSeconds != 90 || restored.Interval != 90*time.Second {
		t.Errorf("interval mismatch: seconds=%d interval=%v", restored.IntervalSeconds, restored.Interval)
	}
	if restored.Rounds != 5 {
		t.Errorf("rounds mismatch: %d", restored.Rounds)
	}
	if !restored.ChannelsEnabled[SchedulerChannelAppleAccount] || !restored.ChannelsEnabled[SchedulerChannelICloudWeb] {
		t.Errorf("channels mismatch: %v", restored.ChannelsEnabled)
	}
	if restored.Status != "paused" {
		t.Errorf("status mismatch: %q, want paused", restored.Status)
	}
	if !restored.CreatedAt.Equal(job.CreatedAt) {
		t.Errorf("created_at mismatch: %v vs %v", restored.CreatedAt, job.CreatedAt)
	}
	if restored.RoundsCompleted != 1 || restored.CreatedCount != 2 {
		t.Errorf("counts mismatch: rounds=%d created=%d, want 1/2", restored.RoundsCompleted, restored.CreatedCount)
	}
	if !restored.isSkipped("acc-2") {
		t.Error("skipped account not restored")
	}
	if restored.isSkipped("acc-1") {
		t.Error("non-skipped account should not be restored as skipped")
	}
	if len(restored.Events) == 0 {
		t.Error("events not restored")
	}

	for _, j := range loaded {
		if j.ID == doneJob.ID {
			if j.Status != "done" {
				t.Errorf("done job status after restore = %q, want done", j.Status)
			}
			if j.RoundsCompleted != 1 || j.CreatedCount != 1 {
				t.Errorf("done job counts = %d/%d, want 1/1", j.RoundsCompleted, j.CreatedCount)
			}
		}
	}
}

// TestSchedulerPersistAtomicWriteAndCorruptFile 验证原子写不残留临时文件,
// 损坏文件/损坏条目容错 (报错或跳过, 不 panic)。
func TestSchedulerPersistAtomicWriteAndCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scheduler-jobs.json")
	sched := newTestScheduler(newFakeCreateFn())
	sched.SetPersistPath(path)
	job := sched.createJob("atomic", []string{"acc-1"}, time.Minute, 0, defaultChannels())
	if err := sched.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if job.ID == "" {
		t.Fatal("job id empty")
	}
	if _, err := LoadJobs(path); err != nil {
		t.Fatalf("LoadJobs after persist: %v", err)
	}
	// 原子写: 目录中不应残留临时文件
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}

	// 整体损坏 (非法 JSON): LoadJobs / RestoreAndStartPersisted 报错且不 panic
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJobs(corrupt); err == nil {
		t.Error("LoadJobs on corrupt file should return error")
	}
	s2 := newTestScheduler(newFakeCreateFn())
	if _, err := s2.RestoreAndStartPersisted(corrupt, context.Background()); err == nil {
		t.Error("RestoreAndStartPersisted on corrupt file should return error")
	}

	// 半损坏: 一条有效 + 一条缺 ID → 跳过坏条目, 恢复有效条目
	mixed := filepath.Join(dir, "mixed.json")
	mixedJSON := `{"version":1,"saved_at":"2026-01-01T00:00:00Z","jobs":[` +
		`{"id":"sched_mixed","name":"ok","account_ids":["a"],"interval_seconds":30,"rounds":0,` +
		`"channels":{},"status":"paused","created_at":"2026-01-01T00:00:00Z","events":[]},` +
		`{"id":"","name":"bad"}]}`
	if err := os.WriteFile(mixed, []byte(mixedJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadJobs(mixed)
	if err != nil {
		t.Fatalf("LoadJobs mixed: %v", err)
	}
	if len(loaded) != 1 || loaded[0].ID != "sched_mixed" {
		t.Errorf("mixed restore = %d jobs, want 1 valid entry", len(loaded))
	}
}

// TestSchedulerRestoreRunningJobContinues 验证恢复的 running 任务经
// RestoreAndStartPersisted 启动后能继续跑完剩余轮次 (fake createFn 计数)。
func TestSchedulerRestoreRunningJobContinues(t *testing.T) {
	fake1 := newFakeCreateFn()
	sched1 := newTestScheduler(fake1)
	job := sched1.createJob("继续跑", []string{"acc-1"}, 10*time.Millisecond, 3, defaultChannels())
	path := filepath.Join(t.TempDir(), "scheduler-jobs.json")
	sched1.SetPersistPath(path) // status=running 落盘
	if job.Status != "running" {
		t.Fatalf("new job status = %s, want running", job.Status)
	}

	fake2 := newFakeCreateFn()
	sched2 := newTestScheduler(fake2)
	started, err := sched2.RestoreAndStartPersisted(path, context.Background())
	if err != nil {
		t.Fatalf("RestoreAndStartPersisted: %v", err)
	}
	if started != 1 {
		t.Fatalf("started = %d, want 1", started)
	}
	defer sched2.stopAll()

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, exists := sched2.getJob(job.ID)
		if exists && got.Status == "done" && got.RoundsCompleted == 3 {
			break
		}
		if time.Now().After(deadline) {
			if !exists {
				t.Fatal("restored job missing")
			}
			t.Fatalf("job did not finish: status=%s rounds=%d created=%d", got.Status, got.RoundsCompleted, got.CreatedCount)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fake2.callCount("acc-1") != 3 {
		t.Errorf("acc-1 calls = %d, want 3", fake2.callCount("acc-1"))
	}
	if fake2.createdCount("acc-1") != 3 {
		t.Errorf("acc-1 created = %d, want 3", fake2.createdCount("acc-1"))
	}
}

// TestSchedulerRestoreSkipsIDConflict 验证 ID 冲突的持久化条目被跳过,
// 已有内存任务不被覆盖。
func TestSchedulerRestoreSkipsIDConflict(t *testing.T) {
	fake := newFakeCreateFn()
	sched := newTestScheduler(fake)
	job := sched.createJob("原任务", []string{"acc-1"}, time.Minute, 3, defaultChannels())
	path := filepath.Join(t.TempDir(), "scheduler-jobs.json")
	sched.SetPersistPath(path)

	sched2 := newTestScheduler(newFakeCreateFn())
	sched2.mu.Lock()
	sched2.jobs[job.ID] = &SchedulerJob{ID: job.ID, Name: "内存版", Status: "paused"}
	sched2.mu.Unlock()
	started, err := sched2.RestoreAndStartPersisted(path, context.Background())
	if err != nil {
		t.Fatalf("RestoreAndStartPersisted: %v", err)
	}
	if started != 0 {
		t.Errorf("started = %d, want 0 (conflict skipped)", started)
	}
	got, exists := sched2.getJob(job.ID)
	if !exists {
		t.Fatal("existing job lost")
	}
	if got.Name != "内存版" {
		t.Errorf("existing job replaced: name = %q, want 内存版", got.Name)
	}
}

// TestSchedulerPersistNoPathIsNoOp 验证未设置 persistPath 时 persist 是 no-op。
func TestSchedulerPersistNoPathIsNoOp(t *testing.T) {
	sched := newTestScheduler(newFakeCreateFn())
	sched.createJob("noop", []string{"acc-1"}, time.Minute, 1, defaultChannels())
	if err := sched.persist(); err != nil {
		t.Errorf("persist without path should be no-op, got error: %v", err)
	}
	sched.SetPersistPath("") // 清空路径后同样 no-op
	if err := sched.persist(); err != nil {
		t.Errorf("persist after clearing path should be no-op: %v", err)
	}
}

// TestSchedulerRestoreMissingFileIsEmpty 验证文件不存在时恢复为空且不报错。
func TestSchedulerRestoreMissingFileIsEmpty(t *testing.T) {
	sched := newTestScheduler(newFakeCreateFn())
	n, err := sched.RestoreAndStartPersisted(filepath.Join(t.TempDir(), "nope.json"), context.Background())
	if err != nil {
		t.Fatalf("RestoreAndStartPersisted with missing file: %v", err)
	}
	if n != 0 {
		t.Errorf("restored %d jobs from missing file, want 0", n)
	}
	if len(sched.listJobs()) != 0 {
		t.Errorf("scheduler should stay empty, got %d jobs", len(sched.listJobs()))
	}
}
