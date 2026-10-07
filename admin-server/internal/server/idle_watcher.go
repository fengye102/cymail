// IDLE 增量收件 watcher: 为每个具备 IMAP 凭证 (ICloudEmail + AppPassword) 且
// 库存中存在收件目标的账号维护一条常驻 TLS 连接监听 INBOX, 新邮件按收件人过滤
// 后复用与 CollectFiltered 相同的入库链路写入 store。
//
// 粒度: 每账号一条连接 (而非每收集目标一条)。iCloud 对单账号的并发 IMAP 连接数
// 有限, 每账号只维持一条; 同一连接上的新邮件按收件人匹配到该账号的多个目标后
// 逐个分发入库。连接级断线由 IdleWatchInbox 内部指数退避自动重连。
//
// 与 30s 轮询 (main.go 的 collectInterval → CollectFiltered, 走网页收件授权) 的
// 关系: IDLE 是即时通道, 轮询保留兜底。游标保存在进程内存 (idleAccountState),
// 重启归零 → IDLE 从当前 UIDNEXT 起只收之后的新邮件, 历史由轮询路径补齐, 与
// 轮询无持久游标的行为一致。回调失败退出后按退避重启, 携带上次成功游标,
// 重叠回扫窗口内的重放由 seen 集合 + SaveMessage 幂等键兜底。
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/fulfillment"
	mailclient "icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

const (
	// idleWatcherIdleTimeout 单次 IDLE 时长, 与 mail 包默认一致。
	idleWatcherIdleTimeout = 20 * time.Minute
	// idleWatcherReconnectMax 断线重连退避封顶。
	idleWatcherReconnectMax = 30 * time.Second
	// idleWatcherRetryBase 回调失败后重启 watcher 的起始退避。
	idleWatcherRetryBase = 5 * time.Second
	// idleWatcherRescanInterval 账号/目标变更后的补扫周期。
	idleWatcherRescanInterval = 5 * time.Minute
	// idleWatcherSeenCap 每账号去重集合上限, 超出清空重建 (精确去重由
	// SaveMessage 的幂等键保证, 内存集合只是减少重复写)。
	idleWatcherSeenCap = 8192
)

// idleAccountState 单个账号 watcher 的进程内存状态, 只在所属 goroutine 内读写。
type idleAccountState struct {
	// cursor 是下一待拉 UID (IdleOptions.StartUID 语义); 整批成功入库后才推进,
	// 失败重启时从上次成功游标继续, 重叠窗口 (100 UID) 内的重放靠 seen 去重。
	cursor uint32
	// seen 记录已入库 (Message.ID, MailboxID) 对, 用于重叠回扫与重试去重。
	seen map[string]struct{}
}

func newIdleAccountState() *idleAccountState {
	return &idleAccountState{seen: make(map[string]struct{})}
}

// startIdleMailWatchers 幂等启动 IDLE 增量收件 (并入 startBackgroundLoops 的
// sync.Once 之外再自带一次保护)。启动时扫描一次, 之后按 idleWatcherRescanInterval
// 周期补扫: 为新出现的可收件账号补开 watcher, 并停掉已失去凭证/收件目标的账号
// watcher。未配置 fulfillment 服务时直接跳过。
func (s *Server) startIdleMailWatchers(ctx context.Context) {
	s.idleWatcherOnce.Do(func() {
		if s.fulfillment == nil {
			log.Printf("IDLE 收件 watcher 未启动: 未配置 fulfillment 库存服务")
			return
		}
		s.scanIdleWatchers(ctx)
		go func() {
			ticker := time.NewTicker(idleWatcherRescanInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.scanIdleWatchers(ctx)
				}
			}
		}()
	})
}

// scanIdleWatchers 对比当前可收件账号与运行中的 watcher, 补齐缺失、停掉失效。
func (s *Server) scanIdleWatchers(ctx context.Context) {
	eligible := s.idleEligibleAccounts(ctx)
	s.idleWatchersMu.Lock()
	defer s.idleWatchersMu.Unlock()
	if s.idleWatchers == nil {
		s.idleWatchers = make(map[string]context.CancelFunc)
	}
	for accountID, cancel := range s.idleWatchers {
		if _, ok := eligible[accountID]; !ok {
			cancel() // 账号失去凭证/目标: 优雅停止该 watcher (IDLE 中断并 Logout)
			delete(s.idleWatchers, accountID)
		}
	}
	for accountID, acc := range eligible {
		if _, ok := s.idleWatchers[accountID]; ok {
			continue
		}
		watcherCtx, cancel := context.WithCancel(ctx)
		s.idleWatchers[accountID] = cancel
		go s.startAccountIdleWatcher(watcherCtx, accountID, acc)
	}
}

// idleEligibleAccounts 返回应启动 IDLE watcher 的账号: 已配置 IMAP 凭证
// (ICloudEmail + AppPassword) 且该账号在库存中存在收件目标 (available/reserved)。
// 注意 Manager.ListAccounts 返回脱敏副本 (不含 AppPassword), 这里用
// Manager.GetAccount 取全量凭证, 无需新增 manager 方法。
func (s *Server) idleEligibleAccounts(ctx context.Context) map[string]*account.Account {
	out := make(map[string]*account.Account)
	if s.fulfillment == nil || s.mgr == nil {
		return out
	}
	accounts := s.mgr.ListAccounts()
	if len(accounts) == 0 {
		return out
	}
	targets, err := s.fulfillment.CollectTargets(ctx)
	if err != nil {
		log.Printf("IDLE 收件 watcher: 读取收件目标失败, 本次扫描不启动新 watcher: %v", err)
		return out
	}
	hasTarget := make(map[string]bool, len(targets))
	for _, target := range targets {
		hasTarget[target.AccountID] = true
	}
	for _, view := range accounts {
		full, ok := s.mgr.GetAccount(view.ID)
		if !ok {
			continue
		}
		if strings.TrimSpace(full.ICloudEmail) == "" || full.AppPassword == "" {
			continue
		}
		if !hasTarget[full.ID] {
			continue
		}
		out[full.ID] = full
	}
	return out
}

// startAccountIdleWatcher 启动单个账号的 watcher goroutine; 测试通过
// Server.idleWatchRunFn 注入替身以观测启动行为。
func (s *Server) startAccountIdleWatcher(ctx context.Context, accountID string, acc *account.Account) {
	if s.idleWatchRunFn != nil {
		s.idleWatchRunFn(ctx, s, accountID, acc)
		return
	}
	s.runIdleWatcher(ctx, accountID, acc)
}

// runIdleWatcher 单账号常驻收件循环。每条连接独占 (IdleWatchInbox 运行期间不
// 与其他方法共用), 退出条件: ctx 取消 (服务关闭/账号失效) 或回调错误。回调错误
// (如 store 暂不可用) 记录日志后按指数退避重启, 从上次成功游标继续; 凭证失效
// 会由 IdleWatchInbox 内部重连退避反复尝试并各自记录, 不影响其他账号 goroutine。
func (s *Server) runIdleWatcher(ctx context.Context, accountID string, acc *account.Account) {
	state := newIdleAccountState()
	backoff := idleWatcherRetryBase
	for {
		if ctx.Err() != nil {
			return
		}
		mc := mailclient.NewClient(acc.ICloudEmail, acc.AppPassword)
		err := mc.IdleWatchInbox(ctx, mailclient.IdleOptions{
			StartUID:     state.cursor,
			IdleTimeout:  idleWatcherIdleTimeout,
			ReconnectMax: idleWatcherReconnectMax,
		}, func(messages []mailclient.Message, nextUID uint32) error {
			if err := s.idleHandleBatch(ctx, accountID, state, messages); err != nil {
				return err
			}
			state.cursor = nextUID // 整批成功后才推进游标
			backoff = idleWatcherRetryBase
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("IDLE 收件 watcher: 账号 %s 的监听已退出, 将从游标 %d 续跑: %v", accountID, state.cursor, err)
		}
		if !idleWatcherSleep(ctx, backoff) {
			return
		}
		if backoff < idleWatcherReconnectMax {
			backoff *= 2
			if backoff > idleWatcherReconnectMax {
				backoff = idleWatcherReconnectMax
			}
		}
	}
}

func idleWatcherSleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// idleHandleBatch 处理一批新邮件: 只入库属于该账号收件目标集合的邮件。收件人
// 匹配语义与轮询路径 (mail.recipientMatches) 一致: 对 To 头做忽略大小写的包含
// 匹配, 兼容 "Display Name <addr>" 形式。按 (Message.ID, MailboxID) 去重 (一封
// 邮件可同时发给多个目标邮箱, 每个目标各存一行)。转换与入库复用
// fulfillment.ConvertIMAPMessage / Service.SaveMessage。任一封保存失败返回错误,
// 使 IdleWatchInbox 退出, watcher 从上次成功游标重启, 该批不丢。
func (s *Server) idleHandleBatch(ctx context.Context, accountID string, state *idleAccountState, messages []mailclient.Message) error {
	if len(messages) == 0 {
		return nil
	}
	if s.fulfillment == nil {
		return errors.New("IDLE 收件: fulfillment 服务未配置, 无法入库")
	}
	targets, err := s.fulfillment.CollectTargets(ctx)
	if err != nil {
		return fmt.Errorf("IDLE 收件: 读取收集目标失败: %w", err)
	}
	byAddress := make(map[string][]store.CollectTarget)
	for _, target := range targets {
		if target.AccountID != accountID {
			continue
		}
		address := strings.ToLower(strings.TrimSpace(target.MailboxAddress))
		if address == "" {
			continue
		}
		byAddress[address] = append(byAddress[address], target)
	}
	saved := 0
	for i := range messages {
		item := &messages[i]
		if item.ID == "" {
			continue // 无 UID 无法去重/幂等, 跳过 (防御)
		}
		toLower := strings.ToLower(item.To)
		for address, targets := range byAddress {
			if !strings.Contains(toLower, address) {
				continue
			}
			for _, target := range targets {
				key := item.ID + "\x00" + target.MailboxID
				if _, ok := state.seen[key]; ok {
					continue
				}
				message := fulfillment.ConvertIMAPMessage(target, *item)
				if err := s.fulfillment.SaveMessage(ctx, message); err != nil {
					return fmt.Errorf("IDLE 收件: 保存邮件 uid=%s (邮箱 %s) 失败: %w", item.ID, target.MailboxID, err)
				}
				state.seen[key] = struct{}{}
				saved++
			}
		}
	}
	if len(state.seen) > idleWatcherSeenCap {
		state.seen = make(map[string]struct{})
	}
	if saved > 0 {
		log.Printf("IDLE 收件: 账号 %s 本批入库 %d 封", accountID, saved)
	}
	return nil
}
