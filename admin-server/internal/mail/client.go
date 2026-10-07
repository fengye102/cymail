// Package mail 实现 iCloud 邮件 IMAP 读取客户端。
//
// 通过 Apple 应用专用密码连接 imap.mail.me.com:993,
// 拉取隐私邮箱别名收到的邮件。对应原 Python 项目 icloud_mail.py。
package mail

import (
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/charset"
)

const (
	IMAPServer = "imap.mail.me.com"
	IMAPPort   = 993
)

// Message 是一封邮件的摘要信息。
type Message struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	Date    string `json:"date"`
	Preview string `json:"preview"`
	// BodyHTML contains the sanitized original HTML body when the provider
	// exposes one. Preview remains the readable plain-text representation.
	BodyHTML    string `json:"body_html,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

// FullMessage 是一封邮件的完整内容(含正文)。
type FullMessage struct {
	Message
	Body string `json:"body"`
}

// Client 是 iCloud 邮件 IMAP 客户端。
type Client struct {
	host     string
	port     int
	username string
	password string
	cli      *client.Client
	// dialOverride 仅供测试注入拨号方式(如明文 client.Dial 连接内存假服务器);
	// 为 nil 时 Connect 使用 client.DialTLS。
	dialOverride func(addr string) (*client.Client, error)
}

// NewClient 创建 IMAP 客户端。需在调用其它方法前先 Connect。
func NewClient(appleID, appPassword string) *Client {
	return NewIMAPClient(IMAPServer, IMAPPort, appleID, appPassword)
}

// NewIMAPClient creates a TLS IMAP client for any supported forwarding mailbox.
func NewIMAPClient(host string, port int, username, password string) *Client {
	if port <= 0 {
		port = IMAPPort
	}
	return &Client{host: strings.TrimSpace(host), port: port, username: strings.TrimSpace(username), password: password}
}

// Connect 连接并登录 IMAP 服务器。
func (c *Client) Connect() error {
	if c.host == "" || c.username == "" || c.password == "" {
		return fmt.Errorf("IMAP 主机、用户名和密码不能为空")
	}
	addr := fmt.Sprintf("%s:%d", c.host, c.port)
	cli, err := c.dialIMAP(addr)
	if err != nil {
		return fmt.Errorf("IMAP 连接失败: %w", err)
	}
	if err := cli.Login(c.username, c.password); err != nil {
		_ = cli.Logout()
		return fmt.Errorf("IMAP 登录失败，请检查授权码或密码以及用户名 %s: %w", c.username, err)
	}
	c.cli = cli
	return nil
}

// dialIMAP 建立 IMAP 连接;测试可通过 dialOverride 注入明文拨号。
func (c *Client) dialIMAP(addr string) (*client.Client, error) {
	if c.dialOverride != nil {
		return c.dialOverride(addr)
	}
	return client.DialTLS(addr, nil)
}

// Disconnect 登出并关闭连接。
func (c *Client) Disconnect() {
	if c.cli != nil {
		_ = c.cli.Logout()
		c.cli = nil
	}
}

// InboxCount 返回收件箱邮件总数。
func (c *Client) InboxCount() (int, error) {
	if c.cli == nil {
		return 0, fmt.Errorf("未连接")
	}
	mbox, err := c.cli.Select("INBOX", false)
	if err != nil {
		return 0, err
	}
	return int(mbox.Messages), nil
}

// ListInbox 拉取收件箱最近 limit 封邮件摘要。
//
// days 用于过滤只看近 N 天的邮件(0 表示不限制)。
// 返回按时间倒序排列。
func (c *Client) ListInbox(limit int, days int) ([]Message, error) {
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}
	if limit <= 0 {
		limit = 50
	}

	mbox, err := c.cli.Select("INBOX", true)
	if err != nil {
		return nil, err
	}
	total := int(mbox.Messages)
	if total == 0 {
		return []Message{}, nil
	}

	// 计算起始序号(只取最近 limit 封)
	from := uint32(1)
	if uint32(limit) < mbox.Messages {
		from = mbox.Messages - uint32(limit) + 1
	}

	seqset := new(imap.SeqSet)
	seqset.AddRange(from, mbox.Messages)

	// 拉取完整正文,以便填充 Preview(OTP 验证码在正文中)
	section := &imap.BodySectionName{}
	items := []imap.FetchItem{
		imap.FetchUid,
		imap.FetchEnvelope,
		imap.FetchInternalDate,
		section.FetchItem(),
	}

	messages := make(chan *imap.Message, limit)
	done := make(chan error, 1)
	go func() {
		done <- c.cli.Fetch(seqset, items, messages)
	}()

	var out []Message
	for msg := range messages {
		m := toMessageWithBody(msg)
		// days 过滤
		if days > 0 {
			if t, err := time.Parse(time.RFC1123Z, m.Date); err == nil {
				if time.Since(t) > time.Duration(days)*24*time.Hour {
					continue
				}
			}
		}
		out = append(out, m)
	}
	if err := <-done; err != nil {
		return nil, err
	}
	return out, nil
}

// FindByRecipient 查找发给指定隐私邮箱别名的邮件。
//
// 先尝试 IMAP TO 搜索;失败则拉取收件箱后本地过滤。
func (c *Client) FindByRecipient(recipient string, limit int, days int) ([]Message, error) {
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}
	if limit <= 0 {
		limit = 20
	}

	// 先尝试服务端 TO 搜索
	mbox, err := c.cli.Select("INBOX", true)
	if err != nil {
		return nil, err
	}
	criteria := imap.NewSearchCriteria()
	criteria.Header.Add("To", recipient)
	if days > 0 {
		since := time.Now().AddDate(0, 0, -days)
		criteria.Since = since
	}
	uids, err := c.cli.UidSearch(criteria)
	if err == nil && len(uids) > 0 {
		return c.fetchByUIDs(uids, limit)
	}
	_ = mbox

	// fallback: 拉取收件箱后本地过滤
	all, err := c.ListInbox(limit*3, days)
	if err != nil {
		return nil, err
	}
	recipient = strings.ToLower(recipient)
	var out []Message
	for _, m := range all {
		if strings.Contains(strings.ToLower(m.To), recipient) {
			out = append(out, m)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (c *Client) fetchByUIDs(uids []uint32, limit int) ([]Message, error) {
	if len(uids) == 0 {
		return []Message{}, nil
	}
	// 取最近 limit 条(UID 倒序)
	if len(uids) > limit {
		uids = uids[len(uids)-limit:]
	}
	seqset := new(imap.SeqSet)
	for _, uid := range uids {
		seqset.AddNum(uid)
	}

	// 拉取完整正文,以便填充 Preview(OTP 验证码在正文中)
	section := &imap.BodySectionName{}
	items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, imap.FetchInternalDate, section.FetchItem()}
	messages := make(chan *imap.Message, len(uids))
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

// GetFull 获取单封邮件的完整内容(含正文)。
func (c *Client) GetFull(uid uint32) (*FullMessage, error) {
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}
	if _, err := c.cli.Select("INBOX", true); err != nil {
		return nil, err
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(uid)

	items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, imap.FetchInternalDate, imap.FetchRFC822}
	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.cli.UidFetch(seqset, items, messages)
	}()

	msg := <-messages
	if err := <-done; err != nil {
		return nil, err
	}
	if msg == nil {
		return nil, fmt.Errorf("邮件不存在 (uid=%d)", uid)
	}

	full := &FullMessage{Message: toMessage(msg)}
	// 解析正文
	if r := msg.GetBody(&imap.BodySectionName{}); r != nil {
		if em, err := mail.ReadMessage(r); err == nil {
			body, _ := readBody(em)
			full.Body = body
			full.ContentType = em.Header.Get("Content-Type")
		}
	}
	return full, nil
}

// ---- 解析工具 ----

func toMessage(msg *imap.Message) Message {
	m := Message{}
	if msg.Uid > 0 {
		m.ID = fmt.Sprintf("%d", msg.Uid)
	}
	if msg.Envelope != nil {
		if len(msg.Envelope.From) > 0 {
			m.From = msg.Envelope.From[0].Address()
		}
		if len(msg.Envelope.To) > 0 {
			addrs := make([]string, 0, len(msg.Envelope.To))
			for _, a := range msg.Envelope.To {
				addrs = append(addrs, a.Address())
			}
			m.To = strings.Join(addrs, ", ")
		}
		m.Subject = decodeHeader(msg.Envelope.Subject)
		if !msg.Envelope.Date.IsZero() {
			m.Date = msg.Envelope.Date.Format(time.RFC3339)
		}
	}
	if m.From != "" {
		m.From = decodeHeader(m.From)
	}
	if m.To != "" {
		m.To = decodeHeader(m.To)
	}
	return m
}

// toMessageWithBody 在 toMessage 基础上解析正文填充 Preview(供 OTP 提取)。
func toMessageWithBody(msg *imap.Message) Message {
	m := toMessage(msg)
	if r := msg.GetBody(&imap.BodySectionName{}); r != nil {
		if em, err := mail.ReadMessage(r); err == nil {
			if body, err := readBody(em); err == nil {
				m.Preview = strings.TrimSpace(body)
			}
		}
	}
	return m
}

// decodeHeader 解码 RFC 2047 编码的邮件头(如 =?UTF-8?B?xxx?=)。
func decodeHeader(s string) string {
	if s == "" {
		return ""
	}
	dec := mime.WordDecoder{CharsetReader: charset.Reader}
	out, err := dec.DecodeHeader(s)
	if err != nil {
		return s
	}
	return out
}

var htmlTag = regexp.MustCompile(`<[^>]+>`)

// readBody 读取邮件正文,优先 text/plain,其次从 HTML 提取纯文本。
func readBody(msg *mail.Message) (string, error) {
	ct := msg.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/html") {
		raw, _ := io.ReadAll(msg.Body)
		// quoted-printable 解码
		if strings.Contains(msg.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
			r := quotedprintable.NewReader(strings.NewReader(string(raw)))
			raw, _ = io.ReadAll(r)
		}
		return stripHTML(string(raw)), nil
	}
	// 默认当 text/plain
	raw, err := io.ReadAll(msg.Body)
	if err != nil {
		return "", err
	}
	if strings.Contains(msg.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
		r := quotedprintable.NewReader(strings.NewReader(string(raw)))
		raw, _ = io.ReadAll(r)
	}
	return string(raw), nil
}

// stripHTML 粗略剥离 HTML 标签,保留可读文本。
func stripHTML(html string) string {
	// 换行标签转换行
	html = strings.ReplaceAll(html, "<br>", "\n")
	html = strings.ReplaceAll(html, "<br/>", "\n")
	html = strings.ReplaceAll(html, "<br />", "\n")
	html = strings.ReplaceAll(html, "</p>", "\n")
	html = strings.ReplaceAll(html, "</div>", "\n")
	html = strings.ReplaceAll(html, "</tr>", "\n")
	html = strings.ReplaceAll(html, "<li>", "\n- ")
	// 去掉所有标签
	html = htmlTag.ReplaceAllString(html, "")
	// 反转义常见实体
	html = strings.ReplaceAll(html, "&nbsp;", " ")
	html = strings.ReplaceAll(html, "&amp;", "&")
	html = strings.ReplaceAll(html, "&lt;", "<")
	html = strings.ReplaceAll(html, "&gt;", ">")
	// 压缩多余空白
	lines := strings.Split(html, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
