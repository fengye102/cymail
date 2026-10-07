// 机器对机 (machine-to-machine) 自动取号与按关键字取码接口。
//
// 两个接口与 /api 其余接口一致, 挂载在管理员认证组内 (authenticate 中间件,
// Bearer / X-API-Key 使用 Config.APIKey):
//
//	POST /api/v1/mailboxes/claim        — 原子领取一个可用邮箱
//	GET  /api/v1/mailboxes/:email/code  — 按关键字/时间过滤提取验证码(只读)
//
// 路由由主 agent 在 server.go 的 register() 中、api 组块末尾追加一行:
//
//	s.registerMachineAPI(api)
//
// 领取复用 fulfillment.Service.AllocateMailboxWithPickupKey
// (→ store.AllocateMailbox), 即参考项目 icloud-privacy-mail 的
// ClaimAvailableMailbox "可用 → 已用" 原子迁移语义: store 在内存锁 /
// Postgres 串行事务 + FOR UPDATE SKIP LOCKED 内把邮箱从 available 标记为
// reserved 并创建订单, 并发下每个邮箱只被领取一次。订单携带随机取件密钥
// 哈希 (Postgres pickup_token_hash/pickup_code_hash 为 NOT NULL),
// 明文密钥不返回给任何人, 邮箱仅作为 "已领取" 记录。
// 取码复用 store.ListMessages 的关键字搜索 (忽略大小写, 覆盖发件人/收件人/
// 主题/正文), 验证码正则复刻参考项目 extractOTP, 全程不修改邮件状态。
package server

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"icloud-hme/internal/store"
)

const (
	machineClaimTTL         = 24 * time.Hour
	machineMessageScanLimit = 500
	machineKeywordWindow    = 160 // keyword 附近取码窗口(字符数)
	machineMaxWaitMS        = 30000
)

// 验证码正则, 复刻参考项目 icloud-privacy-mail server.go 的 extractOTP:
//  1. 上下文正则: openai/chatgpt/otp/code/verification/验证码/验证/代码
//     等关键字后 80 个非数字字符内出现 6 位数字, 覆盖 "code is 123456" 这类;
//  2. 兜底: 正文中任意独立的 6 位数字。
var (
	machineContextOTPRegex = regexp.MustCompile(`(?i)(?:openai|chatgpt|otp|code|verification|验证码|验证|代码)[^\d]{0,80}(\d{6})`)
	machinePlainOTPRegex   = regexp.MustCompile(`\b(\d{6})\b`)
)

// registerMachineAPI 将机器对机接口注册到已带认证/安全中间件的 API 组下
// (子组 /v1/mailboxes, 最终形如 /api/v1/mailboxes/...)。
func (s *Server) registerMachineAPI(api *gin.RouterGroup) {
	v1 := api.Group("/v1/mailboxes")
	v1.POST("/claim", s.machineClaimMailbox)
	v1.GET("/:email/code", s.machineGetMailboxCode)
}

// ====================================================================
// POST /api/v1/mailboxes/claim
//
// 请求体(可选): {"account_ids": ["acc_xxx", ...], "keyword": "shop"}
//   - account_ids: 只从指定账号的库存中领取; 空/缺省 = 任意账号
//   - keyword:     忽略大小写匹配邮箱地址或标签 (Label); 空/缺省 = 任意
//   - 请求体缺省或为空对象 = 领取任意可用邮箱
//
// 响应 200: {mailbox_id, email, forward_to, label, account_id,
//            created_at, claimed_at}
// 错误:
//   401 未认证(与 /api 一致)
//   400 请求体不是合法 JSON
//   409 暂无可用邮箱库存 / 暂无符合条件的可用邮箱
//   503 库存服务未配置 (requireFulfillment)
//   500 其他失败
// ====================================================================

type machineClaimReq struct {
	AccountIDs []string `json:"account_ids"`
	Keyword    string   `json:"keyword"`
}

func (s *Server) machineClaimMailbox(c *gin.Context) {
	if !s.requireFulfillment(c) {
		return
	}
	var req machineClaimReq
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			fail(c, http.StatusBadRequest, "请求格式错误: "+err.Error())
			return
		}
	}
	ctx := c.Request.Context()
	accountIDs := machineNormalizeAccountIDs(req.AccountIDs)
	keyword := strings.ToLower(strings.TrimSpace(req.Keyword))
	externalID := "machine-claim:" + uuid.NewString()

	var claimed *store.Mailbox
	if len(accountIDs) == 0 && keyword == "" {
		// 无筛选: 直接走 store 的原子 "最旧可用 → reserved" 迁移
		order, _, err := s.fulfillment.AllocateMailboxWithPickupKey(ctx, "", externalID, machineClaimTTL)
		if errors.Is(err, store.ErrNoInventory) {
			fail(c, http.StatusConflict, "暂无可用邮箱库存")
			return
		}
		if err != nil {
			fail(c, http.StatusInternalServerError, "领取失败: "+err.Error())
			return
		}
		mailbox, lookupErr := s.machineLookupMailbox(ctx, order.MailboxID)
		if lookupErr != nil {
			fail(c, http.StatusInternalServerError, "领取失败: "+lookupErr.Error())
			return
		}
		claimed = mailbox
	} else {
		// 有筛选: 先列出候选, 再逐个尝试原子领取; 并发下若候选已被
		// 其他请求领走 (ErrNoInventory), 顺延下一个候选, 保证每个
		// 邮箱只被领取一次。
		candidates, err := s.machineClaimCandidates(ctx, accountIDs, keyword)
		if err != nil {
			fail(c, http.StatusInternalServerError, "领取失败: "+err.Error())
			return
		}
		for index := range candidates {
			_, _, err := s.fulfillment.AllocateMailboxWithPickupKey(ctx, candidates[index].ID, externalID, machineClaimTTL)
			if errors.Is(err, store.ErrNoInventory) {
				continue
			}
			if err != nil {
				fail(c, http.StatusInternalServerError, "领取失败: "+err.Error())
				return
			}
			mailbox := candidates[index]
			claimed = &mailbox
			break
		}
		if claimed == nil {
			fail(c, http.StatusConflict, "暂无符合条件的可用邮箱")
			return
		}
	}

	ok(c, gin.H{
		"mailbox_id": claimed.ID,
		"email":      claimed.Address,
		"forward_to": claimed.ForwardToEmail,
		"label":      claimed.Label,
		"account_id": claimed.AccountID,
		"created_at": claimed.CreatedAt,
		"claimed_at": time.Now(),
	})
}

// ====================================================================
// GET /api/v1/mailboxes/:email/code?keyword=&after=&wait_ms=
//
//   email    — 库存中的邮箱地址(不区分大小写)
//   keyword  — 过滤邮件主题/正文(忽略大小写, 复用 store 关键字搜索)
//   after    — RFC3339 时间, 只返回该时间之后的邮件
//   wait_ms  — 0-30000; >0 时以约 1s 间隔轮询等待新邮件, 超时返回 404
//
// 响应 200: {email, code, keyword, message_id, received_at, subject,
//            from, matched_fragment}
// 错误:
//   401 未认证(与 /api 一致)
//   400 after 非法 / wait_ms 非法
//   404 邮箱不存在或不在库存中 / 暂未收到验证码邮件(含 wait_ms 超时)
//   503 库存服务未配置
//   500 其他失败
//
// 只读操作: 不改变邮件状态, 也不触发收件同步。
// ====================================================================

type machineCodeResult struct {
	Message  store.Message
	Code     string
	Fragment string
}

func (s *Server) machineGetMailboxCode(c *gin.Context) {
	if !s.requireFulfillment(c) {
		return
	}
	ctx := c.Request.Context()
	email := strings.TrimSpace(c.Param("email"))
	if email == "" {
		fail(c, http.StatusBadRequest, "邮箱地址不能为空")
		return
	}
	keyword := strings.TrimSpace(c.Query("keyword"))
	after, err := machineParseAfter(c.Query("after"))
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	waitMS, err := machineParseWaitMS(c.Query("wait_ms"))
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}

	mailbox, err := s.machineLookupMailboxByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, errMachineMailboxNotFound) {
			fail(c, http.StatusNotFound, "邮箱不存在或不在库存中")
			return
		}
		fail(c, http.StatusInternalServerError, "查询邮箱失败: "+err.Error())
		return
	}

	deadline := time.Now().Add(time.Duration(waitMS) * time.Millisecond)
	for {
		result, err := s.machineFindCodeMessage(ctx, mailbox, keyword, after)
		if err != nil {
			fail(c, http.StatusInternalServerError, "读取邮件失败: "+err.Error())
			return
		}
		if result != nil {
			ok(c, gin.H{
				"email":            mailbox.Address,
				"code":             result.Code,
				"keyword":          keyword,
				"message_id":       result.Message.ID,
				"received_at":      result.Message.ReceivedAt,
				"subject":          result.Message.Subject,
				"from":             result.Message.Sender,
				"matched_fragment": result.Fragment,
			})
			return
		}
		if waitMS <= 0 || !time.Now().Before(deadline) {
			break
		}
		sleep := time.Second
		if remaining := time.Until(deadline); remaining < sleep {
			sleep = remaining
		}
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return // 客户端已断开, 无需再写响应
		case <-timer.C:
		}
	}
	fail(c, http.StatusNotFound, "暂未收到验证码邮件")
}

// ---- 领取辅助 ----

func machineNormalizeAccountIDs(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// machineClaimCandidates 列出 available 且符合 account_ids/keyword 的候选。
// keyword 忽略大小写匹配邮箱地址或标签; 候选按创建时间升序, 与 store
// "最旧优先" 的分配顺序一致。
func (s *Server) machineClaimCandidates(ctx context.Context, accountIDs []string, keyword string) ([]store.Mailbox, error) {
	items, err := s.fulfillment.ListMailboxes(ctx, "available", 1000)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		allowed[id] = struct{}{}
	}
	candidates := make([]store.Mailbox, 0, len(items))
	for _, mailbox := range items {
		if len(allowed) > 0 {
			if _, ok := allowed[mailbox.AccountID]; !ok {
				continue
			}
		}
		if keyword != "" && !strings.Contains(strings.ToLower(mailbox.Address), keyword) && !strings.Contains(strings.ToLower(mailbox.Label), keyword) {
			continue
		}
		candidates = append(candidates, mailbox)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].CreatedAt.Before(candidates[j].CreatedAt) })
	return candidates, nil
}

func (s *Server) machineLookupMailbox(ctx context.Context, mailboxID string) (*store.Mailbox, error) {
	items, err := s.fulfillment.ListMailboxes(ctx, "", 1000)
	if err != nil {
		return nil, err
	}
	for index := range items {
		if items[index].ID == mailboxID {
			mailbox := items[index]
			return &mailbox, nil
		}
	}
	return nil, errors.New("mailbox not found after allocation")
}

// ---- 取码辅助 ----

var errMachineMailboxNotFound = errors.New("mailbox not found in inventory")

func (s *Server) machineLookupMailboxByEmail(ctx context.Context, email string) (*store.Mailbox, error) {
	items, err := s.fulfillment.ListMailboxes(ctx, "", 1000)
	if err != nil {
		return nil, err
	}
	for index := range items {
		if strings.EqualFold(items[index].Address, email) {
			mailbox := items[index]
			return &mailbox, nil
		}
	}
	return nil, errMachineMailboxNotFound
}

// machineFindCodeMessage 复用 store 的关键字搜索 (主题/正文忽略大小写),
// 再按 after 过滤, 对每封邮件做验证码提取; 返回最新一封命中的邮件。
func (s *Server) machineFindCodeMessage(ctx context.Context, mailbox *store.Mailbox, keyword string, after time.Time) (*machineCodeResult, error) {
	messages, err := s.fulfillment.ListUnifiedMessages(ctx, mailbox.ID, "", keyword, machineMessageScanLimit)
	if err != nil {
		return nil, err
	}
	for index := range messages {
		message := messages[index]
		if !after.IsZero() && !message.ReceivedAt.After(after) {
			continue
		}
		text := message.Subject + "\n" + message.BodyText
		code, fragment := machineExtractOTPSmart(text, keyword)
		if code == "" && message.OTPCode != "" {
			// 兜底: 收件收集时已提取的验证码 (参考项目同样优先使用
			// 存储的 OTPCode)
			code = message.OTPCode
			fragment = message.OTPCode
		}
		if code == "" {
			continue
		}
		return &machineCodeResult{Message: message, Code: code, Fragment: fragment}, nil
	}
	return nil, nil
}

func machineParseAfter(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, errors.New("after 必须是 RFC3339 时间")
	}
	return parsed, nil
}

func machineParseWaitMS(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("wait_ms 必须是整数")
	}
	if value < 0 || value > machineMaxWaitMS {
		return 0, errors.New("wait_ms 必须在 0 到 30000 之间")
	}
	return value, nil
}

// ---- 验证码提取 ----

// machineExtractOTPSmart 依次尝试: 上下文正则 → keyword 附近取码 →
// 任意独立 6 位数字。
func machineExtractOTPSmart(text, keyword string) (code, fragment string) {
	if code, fragment := machineExtractOTP(text); code != "" {
		return code, fragment
	}
	if keyword != "" {
		if code, fragment := machineFindOTPCodeNearKeyword(text, keyword); code != "" {
			return code, fragment
		}
	}
	return "", ""
}

// machineExtractOTP 复刻参考项目 extractOTP: 上下文正则优先, 其次正文中
// 任意独立 6 位数字; 排除 000000。返回验证码与命中的文本片段。
func machineExtractOTP(text string) (code, fragment string) {
	if matches := machineContextOTPRegex.FindStringSubmatch(text); len(matches) == 2 && machineValidOTP(matches[1]) {
		return matches[1], matches[0]
	}
	for _, matches := range machinePlainOTPRegex.FindAllStringSubmatch(text, -1) {
		if len(matches) == 2 && machineValidOTP(matches[1]) {
			return matches[1], matches[0]
		}
	}
	return "", ""
}

func machineValidOTP(code string) bool {
	return len(code) == 6 && code != "000000"
}

// machineFindOTPCodeNearKeyword 在 keyword 每次出现位置前后
// machineKeywordWindow 字符内查找验证码, 覆盖参考项目正则未收录的
// 自定义服务关键字场景 (如 keyword=nike 而正文只写 "Nike code: 123456")。
func machineFindOTPCodeNearKeyword(text, keyword string) (code, fragment string) {
	lowerText := strings.ToLower(text)
	lowerKeyword := strings.ToLower(keyword)
	if lowerKeyword == "" {
		return "", ""
	}
	start := 0
	for {
		position := strings.Index(lowerText[start:], lowerKeyword)
		if position < 0 {
			return "", ""
		}
		position += start
		begin := position - machineKeywordWindow
		if begin < 0 {
			begin = 0
		}
		end := position + len(lowerKeyword) + machineKeywordWindow
		if end > len(text) {
			end = len(text)
		}
		if code, fragment := machineExtractOTP(text[begin:end]); code != "" {
			return code, fragment
		}
		start = position + len(lowerKeyword)
	}
}
