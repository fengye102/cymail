package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const fileStoreVersion = 1

// File is a process-local, JSON-backed Store. Mutations are serialized and
// committed with a temporary-file rename so an interrupted write cannot leave
// a partially written database.
type File struct {
	mu     sync.Mutex
	path   string
	memory *Memory
}

type fileState struct {
	Version   int         `json:"version"`
	Mailboxes []Mailbox   `json:"mailboxes"`
	Orders    []fileOrder `json:"orders"`
	Messages  []Message   `json:"messages"`
}

// fileOrder deliberately persists only pickup hashes. The bearer pickup key
// and short pickup code never enter Store and therefore cannot be written.
type fileOrder struct {
	ID              string     `json:"id"`
	ExternalID      string     `json:"external_id,omitempty"`
	MailboxID       string     `json:"mailbox_id"`
	MailboxAddress  string     `json:"mailbox_address"`
	PickupTokenHash []byte     `json:"pickup_token_hash"`
	PickupCodeHash  []byte     `json:"pickup_code_hash"`
	Status          string     `json:"status"`
	ValidForSeconds int64      `json:"valid_for_seconds,omitempty"`
	ActivatedAt     *time.Time `json:"activated_at,omitempty"`
	ExpiresAt       time.Time  `json:"expires_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

var _ Store = (*File)(nil)

// OpenFile loads a file store from path. A missing file starts empty and is
// created by Migrate or the first successful mutation.
func OpenFile(path string) (*File, error) {
	path = filepath.Clean(path)
	if path == "." || filepath.Base(path) == "." {
		return nil, errors.New("file store path is required")
	}
	f := &File{path: path, memory: NewMemory()}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read file store: %w", err)
	}
	if len(raw) == 0 {
		return nil, errors.New("file store is empty")
	}
	var state fileState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("decode file store: %w", err)
	}
	if state.Version != fileStoreVersion {
		return nil, fmt.Errorf("unsupported file store version %d", state.Version)
	}
	f.memory.restore(memoryStateFromFile(state))
	return f, nil
}

func (f *File) Migrate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := os.Stat(f.path); err == nil {
		return os.Chmod(f.path, 0o600)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return f.persistLocked()
}

func (f *File) Close() {}

func (f *File) UpsertMailboxes(ctx context.Context, items []Mailbox) error {
	return f.mutate(ctx, func() error { return f.memory.UpsertMailboxes(ctx, items) })
}

func (f *File) PruneMailboxes(ctx context.Context, accountID string, keepAddresses []string) (int, error) {
	var pruned int
	err := f.mutate(ctx, func() error {
		var err error
		pruned, err = f.memory.PruneMailboxes(ctx, accountID, keepAddresses)
		return err
	})
	return pruned, err
}

func (f *File) DeleteMailboxByAnonymousID(ctx context.Context, accountID, anonymousID string) (bool, error) {
	var deleted bool
	err := f.mutate(ctx, func() error {
		var err error
		deleted, err = f.memory.DeleteMailboxByAnonymousID(ctx, accountID, anonymousID)
		return err
	})
	return deleted, err
}

func (f *File) ListMailboxes(ctx context.Context, status string, limit int) ([]Mailbox, error) {
	return f.memory.ListMailboxes(ctx, status, limit)
}

func (f *File) AllocateMailbox(ctx context.Context, params AllocateParams) (*Order, error) {
	var order *Order
	err := f.mutate(ctx, func() error {
		var err error
		order, err = f.memory.AllocateMailbox(ctx, params)
		return err
	})
	return order, err
}

func (f *File) ListOrders(ctx context.Context, limit int) ([]Order, error) {
	return f.memory.ListOrders(ctx, limit)
}

func (f *File) ReissueOrderPickup(ctx context.Context, orderID string, tokenHash, codeHash []byte, expiresAt time.Time) (*Order, error) {
	var order *Order
	err := f.mutate(ctx, func() error {
		var err error
		order, err = f.memory.ReissueOrderPickup(ctx, orderID, tokenHash, codeHash, expiresAt)
		return err
	})
	return order, err
}

func (f *File) ActivatePickup(ctx context.Context, tokenHash, codeHash []byte) (*Pickup, error) {
	var pickup *Pickup
	err := f.mutate(ctx, func() error {
		var err error
		pickup, err = f.memory.ActivatePickup(ctx, tokenHash, codeHash)
		return err
	})
	return pickup, err
}

func (f *File) SaveMessage(ctx context.Context, message Message) error {
	return f.mutate(ctx, func() error { return f.memory.SaveMessage(ctx, message) })
}

func (f *File) ListMessages(ctx context.Context, mailboxID, accountID, query string, limit int) ([]Message, error) {
	return f.memory.ListMessages(ctx, mailboxID, accountID, query, limit)
}

func (f *File) ListMessagesByOrder(ctx context.Context, orderID string, limit int) ([]Message, error) {
	return f.memory.ListMessagesByOrder(ctx, orderID, limit)
}

func (f *File) ListCollectTargets(ctx context.Context) ([]CollectTarget, error) {
	return f.memory.ListCollectTargets(ctx)
}

func (f *File) Stats(ctx context.Context) (*Stats, error) {
	return f.memory.Stats(ctx)
}

func (f *File) ExpireOrders(ctx context.Context) error {
	return f.mutate(ctx, func() error { return f.memory.ExpireOrders(ctx) })
}

func (f *File) mutate(ctx context.Context, mutation func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	before := f.memory.snapshot()
	if err := mutation(); err != nil {
		return err
	}
	if err := f.persistLocked(); err != nil {
		f.memory.restore(before)
		return err
	}
	// 写时每日备份：幂等，同一天只生成一份备份；备份失败只记录日志，
	// 不影响本次数据写入与事务提交。
	if err := f.backupDailyLocked(); err != nil {
		log.Printf("fulfillment store 每日备份失败: %v", err)
	}
	return nil
}

func (f *File) persistLocked() error {
	state := fileStateFromMemory(f.memory.snapshot())
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create file store directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".fulfillment-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file store: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("set temporary file store permissions: %w", err)
	}
	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(state); err != nil {
		return fmt.Errorf("encode file store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync file store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close file store: %w", err)
	}
	if err := os.Rename(tmpPath, f.path); err != nil {
		return fmt.Errorf("commit file store: %w", err)
	}
	committed = true
	if err := os.Chmod(f.path, 0o600); err != nil {
		return fmt.Errorf("set file store permissions: %w", err)
	}
	return nil
}

// backupDailyLocked 把当前落盘数据文件每日备份到 <同目录>/backups/<basename>-YYYYMMDD.json。
//
// 幂等：同一天目标文件已存在则跳过复制；无论是否复制都执行保留最近 7 份的清理。
// 备份文件以 0600 落盘（临时文件 + rename，避免半截备份）；备份目录权限 0700。
func (f *File) backupDailyLocked() error {
	raw, err := os.ReadFile(f.path)
	if err != nil {
		return fmt.Errorf("read file store for backup: %w", err)
	}
	backupsDir := filepath.Join(filepath.Dir(f.path), "backups")
	if err := os.MkdirAll(backupsDir, 0o700); err != nil {
		return fmt.Errorf("create file store backups directory: %w", err)
	}
	_ = os.Chmod(backupsDir, 0o700)
	base := strings.TrimSuffix(filepath.Base(f.path), filepath.Ext(f.path))
	prefix := base + "-"
	target := filepath.Join(backupsDir, prefix+time.Now().Format("20060102")+".json")
	if _, err := os.Stat(target); err == nil {
		// 幂等：同一天已备份过，仍执行清理
		return pruneFileStoreBackups(backupsDir, prefix, 7)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(backupsDir, "."+prefix+"backup-*.tmp")
	if err != nil {
		return fmt.Errorf("create backup temp file: %w", err)
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
		return fmt.Errorf("write backup temp file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("set backup temp file permissions: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync backup temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close backup temp file: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("commit backup file: %w", err)
	}
	committed = true
	return pruneFileStoreBackups(backupsDir, prefix, 7)
}

// pruneFileStoreBackups 清理备份目录中 <prefix>YYYYMMDD.json 命名的备份，
// 仅保留最近 keep 份（按文件名日期排序，YYYYMMDD 字典序即时间序）。
func pruneFileStoreBackups(dir, prefix string, keep int) error {
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
			return fmt.Errorf("prune old backup %s: %w", name, err)
		}
	}
	return nil
}

type memoryState struct {
	mailboxes map[string]Mailbox
	orders    map[string]Order
	messages  map[string]Message
}

func (m *Memory) snapshot() memoryState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	state := memoryState{
		mailboxes: make(map[string]Mailbox, len(m.mailboxes)),
		orders:    make(map[string]Order, len(m.orders)),
		messages:  make(map[string]Message, len(m.messages)),
	}
	for id, mailbox := range m.mailboxes {
		state.mailboxes[id] = mailbox
	}
	for id, order := range m.orders {
		order.PickupTokenHash = append([]byte(nil), order.PickupTokenHash...)
		order.PickupCodeHash = append([]byte(nil), order.PickupCodeHash...)
		if order.ActivatedAt != nil {
			activatedAt := *order.ActivatedAt
			order.ActivatedAt = &activatedAt
		}
		state.orders[id] = order
	}
	for id, message := range m.messages {
		state.messages[id] = message
	}
	return state
}

func (m *Memory) restore(state memoryState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mailboxes = state.mailboxes
	m.orders = state.orders
	m.messages = state.messages
}

func fileStateFromMemory(state memoryState) fileState {
	out := fileState{Version: fileStoreVersion}
	for _, mailbox := range state.mailboxes {
		out.Mailboxes = append(out.Mailboxes, mailbox)
	}
	for _, order := range state.orders {
		out.Orders = append(out.Orders, fileOrder{
			ID: order.ID, ExternalID: order.ExternalID, MailboxID: order.MailboxID,
			MailboxAddress:  order.MailboxAddress,
			PickupTokenHash: append([]byte(nil), order.PickupTokenHash...),
			PickupCodeHash:  append([]byte(nil), order.PickupCodeHash...),
			Status:          order.Status, ValidForSeconds: order.ValidForSeconds, ActivatedAt: order.ActivatedAt, ExpiresAt: order.ExpiresAt, CreatedAt: order.CreatedAt,
		})
	}
	for _, message := range state.messages {
		out.Messages = append(out.Messages, message)
	}
	sort.Slice(out.Mailboxes, func(i, j int) bool { return out.Mailboxes[i].ID < out.Mailboxes[j].ID })
	sort.Slice(out.Orders, func(i, j int) bool { return out.Orders[i].ID < out.Orders[j].ID })
	sort.Slice(out.Messages, func(i, j int) bool { return out.Messages[i].ID < out.Messages[j].ID })
	return out
}

func memoryStateFromFile(state fileState) memoryState {
	out := memoryState{
		mailboxes: make(map[string]Mailbox, len(state.Mailboxes)),
		orders:    make(map[string]Order, len(state.Orders)),
		messages:  make(map[string]Message, len(state.Messages)),
	}
	for _, mailbox := range state.Mailboxes {
		out.mailboxes[mailbox.ID] = mailbox
	}
	for _, record := range state.Orders {
		validForSeconds := record.ValidForSeconds
		activatedAt := record.ActivatedAt
		if validForSeconds <= 0 {
			validForSeconds = int64(record.ExpiresAt.Sub(record.CreatedAt) / time.Second)
			if validForSeconds <= 0 {
				validForSeconds = int64((24 * time.Hour) / time.Second)
			}
			legacyActivatedAt := record.CreatedAt
			activatedAt = &legacyActivatedAt
		}
		out.orders[record.ID] = Order{
			ID: record.ID, ExternalID: record.ExternalID, MailboxID: record.MailboxID,
			MailboxAddress:  record.MailboxAddress,
			PickupTokenHash: append([]byte(nil), record.PickupTokenHash...),
			PickupCodeHash:  append([]byte(nil), record.PickupCodeHash...),
			Status:          record.Status, ValidForSeconds: validForSeconds, ActivatedAt: activatedAt, ExpiresAt: record.ExpiresAt, CreatedAt: record.CreatedAt,
		}
	}
	for _, message := range state.Messages {
		out.messages[message.ID] = message
	}
	return out
}
