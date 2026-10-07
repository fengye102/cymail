package mail

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

// ---- IDLE 常驻增量收件 ----

const (
	// idleDefaultTimeout 是单次 IDLE 的默认时长。
	idleDefaultTimeout = 20 * time.Minute
	// idleDefaultReconnectMax 是重连退避的默认上限。
	idleDefaultReconnectMax = 30 * time.Second
	// idleBaseBackoff 是重连指数退避的起点(1s→2s→4s…)。
	idleBaseBackoff = time.Second
	// idleCursorOverlap 是首次同步时回扫的 UID 安全余量。参考项目用
	// mailboxSyncCursorOverlap=2 分钟的时间重叠兜住游标边界上可能漏掉的邮件;
	// 这里把同样的语义落到 UID 空间:调用方传入外部游标时,首个周期从
	// "游标 - idleCursorOverlap" 开始重拉,把边界邮件重放给调用方按 ID 去重。
	idleCursorOverlap = 100
	// idleFetchChunk 是单次 UID FETCH 覆盖的 UID 宽度(按 UID 区间分块,
	// 避免重连后的超大回扫一次拉爆内存)。
	idleFetchChunk = 100
	// idleUpdatesBuffer 是 go-imap 未请求更新通道(EXISTS/EXPUNGE 等)的缓冲大小。
	idleUpdatesBuffer = 64
)

// IdleOptions 控制 IdleWatchInbox 的行为。
type IdleOptions struct {
	// StartUID 是上次增量游标(UIDNEXT 起点):0 表示不补拉历史,
	// 自动取当前 UIDNEXT,只收之后到达的新邮件;>0 时首次同步会从
	// max(StartUID-idleCursorOverlap, 1) 起补拉重叠窗口 + [StartUID, UIDNEXT)。
	StartUID uint32
	// IdleTimeout 是单次 IDLE 的时长,默认 20 分钟;超时后自动重新
	// STATUS+IDLE,同时借机刷新 UIDNEXT,避免被服务器登出。
	IdleTimeout time.Duration
	// ReconnectMax 是断线重连指数退避(1s→2s→4s…)的封顶值,默认 30s。
	ReconnectMax time.Duration
}

// IdleMessage 是 IDLE 增量回调中的单封邮件,与 Message 完全一致。
type IdleMessage = Message

// idleHandlerError 包装 handler 返回的错误,用于与网络错误区分。
type idleHandlerError struct{ err error }

func (e *idleHandlerError) Error() string { return "mail: idle handler: " + e.err.Error() }
func (e *idleHandlerError) Unwrap() error { return e.err }

// IdleWatchInbox 常驻监听 INBOX 新邮件。循环执行:
//
//	(重)连接后 SELECT INBOX 取 UIDNEXT → 补拉/增量拉取并逐批回调 → IDLE 等待 → 重复
//
// 稳态循环用 STATUS INBOX (UIDNEXT) 刷新游标而不是反复 SELECT:SELECT 的
// 未请求 EXISTS/RECENT 会被 go-imap 推入更新通道造成自唤醒,STATUS 无此噪声。
//
// 游标语义与 IdleOptions.StartUID 一致:都是 "下一个待拉取的 UID"(UIDNEXT 起点)。
// 增量拉取只取 UID ≥ 游标的邮件。调用方传入外部游标 StartUID>0 时,首个周期会从
// max(StartUID-idleCursorOverlap, 1) 起回扫重叠窗口(等价于参考项目
// mailboxSyncCursorOverlap 的 2 分钟重叠),兜住陈旧游标边界上可能漏掉的邮件,
// 回调里可能出现与历史重复的邮件,由调用方按 Message.ID 去重;此后内部游标精确
// 维护,断线重连从上次游标继续,不再产生重复。
// 每批回调的 nextUID 是处理完该批后的新游标,调用方应持久化它,下次启动时作为
// StartUID 传入,断点即精确续传(不再有重叠重复)。
//
// 回调返回非 nil 错误时本函数立即终止并返回该错误(已 Logout),重试策略由调用方决定。
// ctx 取消时优雅退出:中断 IDLE、Logout,返回 nil。
// 网络异常(服务器断开、IDLE 中断等)按指数退避自动重连,直到 ctx 取消。
//
// 注意:运行期间独占该 Client 的连接,调用方不要并发调用同一 Client 的其它方法。
func (c *Client) IdleWatchInbox(ctx context.Context, opts IdleOptions, handler func(newMessages []Message, nextUID uint32) error) error {
	if handler == nil {
		return errors.New("mail: IdleWatchInbox 的 handler 不能为 nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	opts = normalizeIdleOptions(opts)

	cursor := opts.StartUID
	// 重叠回扫只发生在首次同步周期(且调用方传入了外部游标):外部游标可能陈旧,
	// 需要回扫安全余量兜底;内部游标由本函数精确维护,重连后无需回扫。
	overlapOnBackfill := opts.StartUID > 0
	firstAfterConnect := true
	backoff := idleBaseBackoff
	if opts.ReconnectMax < backoff {
		backoff = opts.ReconnectMax
	}
	var updates chan client.Update
	activity := make(chan struct{}, 1)
	var drainStop, drainDone chan struct{}

	stopDrainer := func() {
		if drainStop != nil {
			close(drainStop)
			<-drainDone
			drainStop, drainDone = nil, nil
		}
	}
	defer stopDrainer()
	defer c.Disconnect()
	dropConnection := func() {
		stopDrainer()
		c.Disconnect()
	}

	for {
		if ctx.Err() != nil {
			return nil // 优雅退出
		}

		// 1. 确保已连接:未连接先 Connect;断开自动重连(指数退避,封顶 ReconnectMax)。
		if c.cli == nil {
			if err := c.Connect(); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if !idleRetryBackoff(ctx, &backoff, opts.ReconnectMax) {
					return nil
				}
				continue
			}
			backoff = idleBaseBackoff
			firstAfterConnect = true
			updates = make(chan client.Update, idleUpdatesBuffer)
			c.cli.Updates = updates
			stopDrainer() // 防御:确保旧连接的排水器已停止
			drainStop, drainDone = startIdleDrainer(ctx, updates, activity)
		} else if updates == nil {
			// 调用方预先 Connect 好的连接:补上更新通道与排水器。
			updates = make(chan client.Update, idleUpdatesBuffer)
			c.cli.Updates = updates
			drainStop, drainDone = startIdleDrainer(ctx, updates, activity)
		}

		var uidNext uint32
		if firstAfterConnect {
			// 2a. (重)连接后的首次周期:SELECT INBOX(只读)取 UIDNEXT,
			//     为 0 时从 UID SEARCH ALL 推导。
			mbox, err := c.cli.Select("INBOX", true)
			if err != nil {
				dropConnection()
				if !idleRetryBackoff(ctx, &backoff, opts.ReconnectMax) {
					return nil
				}
				continue
			}
			uidNext = mbox.UidNext
			if uidNext == 0 {
				uidNext, err = c.uidSearchNext()
				if err != nil {
					dropConnection()
					if !idleRetryBackoff(ctx, &backoff, opts.ReconnectMax) {
						return nil
					}
					continue
				}
			}
			// 清掉 SELECT 自身产生的 EXISTS/RECENT 未请求更新,避免下次 IDLE 自唤醒。
			idleDrainActivity(activity)

			if cursor == 0 {
				cursor = uidNext // 自动起点:从当前 UIDNEXT 开始,不回补历史
			}
			if cursor > uidNext {
				cursor = uidNext // 防御:服务器 UIDNEXT 回退(正常不会发生)
			}
			fetchStart := cursor
			if overlapOnBackfill {
				fetchStart = idleOverlapStart(cursor, idleCursorOverlap)
			}
			firstAfterConnect = false
			if fetchStart < uidNext {
				newCursor, err := c.syncRange(fetchStart, uidNext, handler)
				if err != nil {
					var handlerErr *idleHandlerError
					if errors.As(err, &handlerErr) {
						return handlerErr.err // 回调出错:终止并返回,由调用方决定重试
					}
					dropConnection()
					if !idleRetryBackoff(ctx, &backoff, opts.ReconnectMax) {
						return nil
					}
					continue // 拉取失败:保持重叠标记,重连后重试仍带重叠
				}
				cursor = newCursor
				if ctx.Err() != nil {
					return nil
				}
			}
			overlapOnBackfill = false
			backoff = idleBaseBackoff
		} else {
			// 2b. 稳态周期:STATUS INBOX (UIDNEXT) 刷新游标(无未请求更新噪声)。
			status, err := c.cli.Status("INBOX", []imap.StatusItem{imap.StatusUidNext})
			if err != nil {
				dropConnection()
				if !idleRetryBackoff(ctx, &backoff, opts.ReconnectMax) {
					return nil
				}
				continue
			}
			uidNext = status.UidNext
			if uidNext > cursor {
				newCursor, err := c.syncRange(cursor, uidNext, handler)
				if err != nil {
					var handlerErr *idleHandlerError
					if errors.As(err, &handlerErr) {
						return handlerErr.err
					}
					dropConnection()
					if !idleRetryBackoff(ctx, &backoff, opts.ReconnectMax) {
						return nil
					}
					continue
				}
				cursor = newCursor
				if ctx.Err() != nil {
					return nil
				}
			}
			backoff = idleBaseBackoff
		}

		// 3. IDLE 等待新邮件通知(EXISTS/EXPUNGE)或超时;连接被断开则走重连。
		if err := c.idleOnce(ctx, opts.IdleTimeout, activity); err != nil {
			dropConnection()
			if ctx.Err() != nil {
				return nil
			}
			if !idleRetryBackoff(ctx, &backoff, opts.ReconnectMax) {
				return nil
			}
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		// 回到第 2 步(稳态 STATUS 刷新 UIDNEXT)进入下一轮。
	}
}

// normalizeIdleOptions 填充默认值:IdleTimeout 默认 20 分钟,ReconnectMax 默认 30s。
func normalizeIdleOptions(opts IdleOptions) IdleOptions {
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = idleDefaultTimeout
	}
	if opts.ReconnectMax <= 0 {
		opts.ReconnectMax = idleDefaultReconnectMax
	}
	return opts
}

// idleOverlapStart 计算重叠回扫起点:游标减安全余量,下限 1。
func idleOverlapStart(cursor, margin uint32) uint32 {
	if cursor <= margin {
		return 1
	}
	return cursor - margin
}

// idleChunkEnd 计算 [from, uidNext) 区间内当前块的结束 UID(含),块宽 chunk。
func idleChunkEnd(from, uidNext, chunk uint32) uint32 {
	to := from + chunk - 1
	if to >= uidNext {
		to = uidNext - 1
	}
	return to
}

// idleNextBackoff 指数退避:current*2,封顶 max;current 为 0 时返回 min(base, max)。
func idleNextBackoff(current, base, max time.Duration) time.Duration {
	var next time.Duration
	if current <= 0 {
		next = base
	} else {
		next = current * 2
	}
	if next <= 0 || next > max {
		return max
	}
	return next
}

// idleWaitBackoff 等待退避时长;ctx 取消立即返回 false。
func idleWaitBackoff(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// idleRetryBackoff 等待当前退避并计算下一次退避;返回 false 表示 ctx 已取消。
func idleRetryBackoff(ctx context.Context, backoff *time.Duration, max time.Duration) bool {
	if !idleWaitBackoff(ctx, *backoff) {
		return false
	}
	*backoff = idleNextBackoff(*backoff, idleBaseBackoff, max)
	return true
}

// idleDrainActivity 清空 activity 通道(丢弃 SELECT 自身产生的 EXISTS 噪声)。
func idleDrainActivity(activity chan struct{}) {
	for {
		select {
		case <-activity:
		default:
			return
		}
	}
}

// syncRange 拉取 [start, uidNext) 区间的邮件,按 idleFetchChunk 分块逐批回调。
// onBatch 收到 (本批邮件, 处理完本批后的新游标)。回调出错返回 idleHandlerError。
// 返回处理完整个区间后的游标(正常等于 uidNext)。
func (c *Client) syncRange(start, uidNext uint32, onBatch func([]Message, uint32) error) (uint32, error) {
	cursor := start
	for from := start; from < uidNext; {
		to := idleChunkEnd(from, uidNext, idleFetchChunk)
		messages, err := c.fetchUIDRange(from, to)
		if err != nil {
			return cursor, fmt.Errorf("mail: 增量拉取 UID %d-%d 失败: %w", from, to, err)
		}
		cursor = to + 1
		if len(messages) > 0 {
			if err := onBatch(messages, cursor); err != nil {
				return cursor, &idleHandlerError{err: err}
			}
		}
		from = cursor
	}
	return cursor, nil
}

// fetchUIDRange 拉取 [from, to] 区间的邮件摘要+正文(与 ListInbox 相同的
// FetchUid/FetchEnvelope/FetchInternalDate/BodySectionName 项),复用
// toMessageWithBody 解析,返回按服务器返回顺序排列的邮件。
func (c *Client) fetchUIDRange(from, to uint32) ([]Message, error) {
	seqset := new(imap.SeqSet)
	seqset.AddRange(from, to)

	section := &imap.BodySectionName{}
	items := []imap.FetchItem{
		imap.FetchUid,
		imap.FetchEnvelope,
		imap.FetchInternalDate,
		section.FetchItem(),
	}

	messages := make(chan *imap.Message, idleFetchChunk)
	done := make(chan error, 1)
	go func() {
		done <- c.cli.UidFetch(seqset, items, messages)
	}()

	var out []Message
	for msg := range messages {
		out = append(out, toMessageWithBody(msg))
	}
	if err := <-done; err != nil {
		return nil, err
	}
	return out, nil
}

// uidSearchNext 在 SELECT 未返回 UIDNEXT 时,通过 UID SEARCH ALL 推导:
// 返回最大 UID + 1(空邮箱返回 1)。
func (c *Client) uidSearchNext() (uint32, error) {
	uids, err := c.cli.UidSearch(imap.NewSearchCriteria())
	if err != nil {
		return 0, err
	}
	var max uint32
	for _, uid := range uids {
		if uid > max {
			max = uid
		}
	}
	return max + 1, nil
}

// idleActivityUpdate 判断一条未请求更新是否值得中断 IDLE 立即同步。
// EXISTS/RECENT(MailboxUpdate)、EXPUNGE 和 BYE(StatusUpdate)都会中断;
// 纯 FETCH 更新(如标志位变化)不中断。
func idleActivityUpdate(u client.Update) bool {
	switch u.(type) {
	case *client.MailboxUpdate, *client.ExpungeUpdate, *client.StatusUpdate:
		return true
	}
	return false
}

// startIdleDrainer 启动更新排水器:持续消费 go-imap 的未请求更新通道(防止
// 缓冲满阻塞客户端读取),把值得立即同步的更新折叠成 activity 信号。
// 返回 stop/done 通道,调用方关闭 stop 并等待 done 以停止。
func startIdleDrainer(ctx context.Context, updates chan client.Update, activity chan struct{}) (stop, done chan struct{}) {
	stop = make(chan struct{})
	done = make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case u, ok := <-updates:
				if !ok {
					return
				}
				if idleActivityUpdate(u) {
					select {
					case activity <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	return stop, done
}

// idleOnce 执行一次 IDLE:收到新邮件通知(activity)、超时或 ctx 取消时返回 nil;
// 连接级错误(服务器断开等)返回错误,由调用方决定重连。
func (c *Client) idleOnce(ctx context.Context, timeout time.Duration, activity <-chan struct{}) error {
	wake := make(chan struct{})
	var wakeOnce sync.Once
	wakeFn := func() { wakeOnce.Do(func() { close(wake) }) }

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	stopWake := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-timer.C:
		case <-activity:
		case <-stopWake:
			return
		}
		wakeFn()
	}()

	idleDone := make(chan error, 1)
	go func() {
		idleDone <- c.cli.Idle(wake, &client.IdleOptions{LogoutTimeout: -1})
	}()
	err := <-idleDone
	close(stopWake)
	return err
}
