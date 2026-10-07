package mail

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	xhtml "golang.org/x/net/html"
)

// NeteaseWebClient reads a user-authorized 163 webmail session. It does not
// use IMAP, an app password, or the user's 163 password.
type NeteaseWebClient struct {
	email     string
	cookies   map[string]string
	sessionID string
	host      string
	readURL   string
	http      *http.Client
}

func NewNeteaseWebClient(email string, cookies map[string]string, sessionID, host string) (*NeteaseWebClient, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		host = "mail.163.com"
	}
	if host != "mail.163.com" && !strings.HasSuffix(host, ".mail.163.com") {
		return nil, fmt.Errorf("不受支持的 163 网页主机")
	}
	if len(cookies) == 0 || strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("163 网页会话不完整")
	}
	return &NeteaseWebClient{
		email: strings.ToLower(strings.TrimSpace(email)), cookies: cloneCookies(cookies),
		sessionID: strings.TrimSpace(sessionID), host: host,
		http: &http.Client{Timeout: 20 * time.Second},
	}, nil
}

// ValidateSession performs the smallest harmless authenticated mailbox read.
func (c *NeteaseWebClient) ValidateSession() error {
	_, err := c.listMessages(1, 0)
	return err
}

func (c *NeteaseWebClient) ListInbox(limit, days int) ([]Message, error) {
	if limit <= 0 {
		limit = 20
	}
	items, err := c.listMessages(limit, 0)
	if err != nil {
		return nil, err
	}
	return filterByDays(items, days), nil
}

func (c *NeteaseWebClient) FindByRecipient(recipient string, limit, days int) ([]Message, error) {
	grouped, err := c.FindByRecipients([]string{recipient}, limit, days)
	if err != nil {
		return nil, err
	}
	return grouped[strings.ToLower(strings.TrimSpace(recipient))], nil
}

// FindByRecipients scans the inbox once and only reads message details when
// the list response does not expose the final recipient. This avoids making a
// full inbox request for every Hide My Email alias.
func (c *NeteaseWebClient) FindByRecipients(recipients []string, limit, days int) (map[string][]Message, error) {
	wanted := make(map[string]struct{}, len(recipients))
	result := make(map[string][]Message, len(recipients))
	for _, recipient := range recipients {
		normalized := strings.ToLower(strings.TrimSpace(recipient))
		if normalized != "" {
			wanted[normalized] = struct{}{}
			result[normalized] = nil
		}
	}
	if len(wanted) == 0 {
		return result, nil
	}
	if limit <= 0 {
		limit = 100
	}
	items, err := c.listMessages(limit, 0)
	if err != nil {
		return nil, err
	}
	items = filterByDays(items, days)
	for _, item := range items {
		listMatches := recipientMatches(item.To, wanted)
		if len(listMatches) == 0 && strings.TrimSpace(item.To) != "" {
			continue
		}
		full, readErr := c.ReadMessage(item.ID)
		if readErr == nil {
			// The listMessages response only contains a short summary. Always
			// hydrate matched messages through readMessage so storage and pickup
			// clients receive the complete body instead of the preview.
			if full.From != "" {
				item.From = full.From
			}
			if full.To != "" {
				item.To = full.To
			}
			if full.Subject != "" {
				item.Subject = full.Subject
			}
			if full.Date != "" {
				item.Date = full.Date
			}
			item.Preview = full.Body
			item.BodyHTML = full.BodyHTML
			item.ContentType = full.ContentType
		}
		matches := recipientMatches(item.To+"\n"+item.Preview, wanted)
		if len(matches) == 0 {
			matches = listMatches
		}
		for _, recipient := range matches {
			result[recipient] = append(result[recipient], item)
		}
	}
	return result, nil
}

func (c *NeteaseWebClient) ReadMessage(id string) (*FullMessage, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("163 邮件 ID 为空")
	}
	payload := `<object><string name="id">` + xmlEscape(id) + `</string><boolean name="header">false</boolean><boolean name="returnImageInfo">true</boolean><boolean name="returnAntispamInfo">true</boolean><boolean name="autoName">true</boolean><boolean name="returnReceived">true</boolean></object>`
	raw, err := c.call("mbox:readMessage", payload)
	if err != nil {
		return nil, err
	}
	root, err := parseNeteaseResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("163 邮件正文格式已变化，请重新授权并更新扩展")
	}
	obj := firstObject(root)
	if obj == nil {
		return nil, fmt.Errorf("163 邮件正文响应为空")
	}
	message := messageFromNode(obj)
	if message.ID == "" {
		message.ID = id
	}
	body, contentType, bodyErr := c.readHTMLBody(id)
	if bodyErr != nil {
		return nil, bodyErr
	}
	message.BodyHTML = sanitizeEmailHTML(body)
	message.ContentType = contentType
	return &FullMessage{Message: message, Body: htmlToText(body)}, nil
}

func (c *NeteaseWebClient) readHTMLBody(id string) (string, string, error) {
	readURL, err := c.resolveReadURL()
	if err != nil {
		return "", "", err
	}
	endpoint, err := url.Parse(readURL)
	if err != nil {
		return "", "", fmt.Errorf("163 正文地址无效")
	}
	separator := "?"
	if endpoint.RawQuery != "" {
		separator = "&"
	}
	id = strings.TrimSpace(id)
	if !regexp.MustCompile(`^[A-Za-z0-9:_-]+$`).MatchString(id) {
		return "", "", fmt.Errorf("163 邮件 ID 格式无效")
	}
	// The current 163 reader expects its colon-delimited MID verbatim. The
	// official web client concatenates it directly instead of percent-encoding
	// the colon; encoding it as %3A produces a generic ErrorPage.
	targetURL := readURL + separator + "mid=" + id + "&userType=ud"
	req, err := http.NewRequest(http.MethodGet, targetURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	req.Header.Set("Referer", "https://mail.163.com/js6/main.jsp?sid="+url.QueryEscape(c.sessionID))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Sec-Fetch-Dest", "iframe")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cookie", cookieHeader(c.cookies))
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("读取 163 邮件正文失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", "", fmt.Errorf("读取 163 邮件正文失败: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || bytes.Contains(bytes.ToLower(raw), []byte("login.jsp")) {
		return "", "", fmt.Errorf("163 网页登录已失效，请重新网页登录授权")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("163 邮件正文接口返回 HTTP %d", resp.StatusCode)
	}
	body := c.prepareEmailHTML(string(raw), resp.Request.URL)
	return body, resp.Header.Get("Content-Type"), nil
}

const (
	maxInlineImages    = 20
	maxInlineImageSize = 5 << 20
)

// prepareEmailHTML resolves relative 163 image references against the real
// reader URL. Images hosted by 163 are fetched with the authorized server-side
// session and embedded so pickup users do not need (and never receive) its
// cookies. External images remain remote and are still controlled by the
// pickup page's explicit privacy switch.
func (c *NeteaseWebClient) prepareEmailHTML(raw string, baseURL *url.URL) string {
	loaded := 0
	return rewriteEmailImages(raw, baseURL, func(imageURL *url.URL) (string, bool) {
		if loaded >= maxInlineImages || !c.isProviderImageURL(imageURL) {
			return "", false
		}
		dataURL, ok := c.fetchInlineImage(imageURL)
		if ok {
			loaded++
		}
		return dataURL, ok
	})
}

func rewriteEmailImages(raw string, baseURL *url.URL, loader func(*url.URL) (string, bool)) string {
	doc, err := xhtml.Parse(strings.NewReader(raw))
	if err != nil || baseURL == nil {
		return raw
	}
	var walk func(*xhtml.Node)
	walk = func(node *xhtml.Node) {
		if node.Type == xhtml.ElementNode {
			for index := range node.Attr {
				name := strings.ToLower(node.Attr[index].Key)
				if name != "src" && name != "background" {
					continue
				}
				reference, parseErr := url.Parse(strings.TrimSpace(node.Attr[index].Val))
				if parseErr != nil || reference.Scheme == "cid" || reference.Scheme == "data" || reference.Scheme == "mailto" {
					continue
				}
				resolved := baseURL.ResolveReference(reference)
				if resolved.Scheme != "http" && resolved.Scheme != "https" {
					continue
				}
				if embedded, ok := loader(resolved); ok {
					node.Attr[index].Val = embedded
				} else {
					node.Attr[index].Val = resolved.String()
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	var output strings.Builder
	if err := xhtml.Render(&output, doc); err != nil {
		return raw
	}
	return output.String()
}

func (c *NeteaseWebClient) isProviderImageURL(imageURL *url.URL) bool {
	host := strings.ToLower(imageURL.Hostname())
	return imageURL.Scheme == "https" && (host == c.host || strings.HasSuffix(host, ".mail.163.com"))
}

func (c *NeteaseWebClient) fetchInlineImage(imageURL *url.URL) (string, bool) {
	req, err := http.NewRequest(http.MethodGet, imageURL.String(), nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/png,image/jpeg,image/gif,*/*;q=0.2")
	req.Header.Set("Referer", "https://mail.163.com/js6/main.jsp?sid="+url.QueryEscape(c.sessionID))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/138.0.0.0 Safari/537.36")
	req.Header.Set("Cookie", cookieHeader(c.cookies))
	resp, err := c.http.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !c.isProviderImageURL(resp.Request.URL) {
		return "", false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxInlineImageSize+1))
	if err != nil || len(raw) == 0 || len(raw) > maxInlineImageSize {
		return "", false
	}
	contentType := http.DetectContentType(raw)
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return "", false
	}
	return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(raw), true
}

func (c *NeteaseWebClient) resolveReadURL() (string, error) {
	if c.readURL != "" {
		return c.readURL, nil
	}
	mainURL := url.URL{Scheme: "https", Host: c.host, Path: "/js6/main.jsp"}
	query := mainURL.Query()
	query.Set("sid", c.sessionID)
	query.Set("df", "mail163_letter")
	mainURL.RawQuery = query.Encode()
	req, err := http.NewRequest(http.MethodGet, mainURL.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cookie", cookieHeader(c.cookies))
	req.Header.Set("User-Agent", "Mozilla/5.0 CYMail/1.0")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("读取 163 正文配置失败: %w", err)
	}
	defer resp.Body.Close()
	for _, cookie := range resp.Cookies() {
		if cookie.Name != "" && cookie.Value != "" {
			c.cookies[cookie.Name] = cookie.Value
		}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("读取 163 正文配置失败: %w", err)
	}
	match := regexp.MustCompile(`(?i)["']?readUrl["']?\s*[:=]\s*["']([^"']+)`).FindSubmatch(raw)
	if len(match) != 2 {
		return "", fmt.Errorf("163 网页正文配置不存在，请重新授权")
	}
	reference, err := url.Parse(string(match[1]))
	if err != nil {
		return "", fmt.Errorf("163 网页正文配置无效")
	}
	resolved := mainURL.ResolveReference(reference)
	if resolved.Scheme != "https" || (resolved.Hostname() != c.host && !strings.HasSuffix(resolved.Hostname(), ".mail.163.com")) {
		return "", fmt.Errorf("163 网页正文主机无效")
	}
	c.readURL = resolved.String()
	return c.readURL, nil
}

func htmlToText(raw string) string {
	doc, err := xhtml.Parse(strings.NewReader(raw))
	if err != nil {
		return strings.TrimSpace(raw)
	}
	var output strings.Builder
	var walk func(*xhtml.Node, bool)
	walk = func(node *xhtml.Node, hidden bool) {
		if node.Type == xhtml.ElementNode {
			tag := strings.ToLower(node.Data)
			hidden = hidden || tag == "script" || tag == "style" || tag == "head" || tag == "noscript"
			if !hidden && (tag == "br" || tag == "p" || tag == "div" || tag == "li" || tag == "tr" || tag == "hr") {
				output.WriteByte('\n')
			}
		}
		if !hidden && node.Type == xhtml.TextNode {
			output.WriteString(node.Data)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child, hidden)
		}
		if !hidden && node.Type == xhtml.ElementNode {
			tag := strings.ToLower(node.Data)
			if tag == "p" || tag == "div" || tag == "li" || tag == "tr" {
				output.WriteByte('\n')
			}
		}
	}
	walk(doc, false)
	lines := strings.Split(strings.ReplaceAll(output.String(), "\r", ""), "\n")
	cleaned := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if !blank && len(cleaned) > 0 {
				cleaned = append(cleaned, "")
			}
			blank = true
			continue
		}
		blank = false
		cleaned = append(cleaned, line)
	}
	return strings.TrimSpace(strings.Join(cleaned, "\n"))
}

var unsafeCSSPattern = regexp.MustCompile(`(?i)(?:javascript\s*:|vbscript\s*:|data\s*:\s*text/html|expression\s*\(|behavior\s*:|-moz-binding\s*:)`)

// sanitizeEmailHTML keeps normal email markup, inline CSS and image metadata,
// while removing active content. The pickup UI adds a sandboxed iframe and a
// restrictive CSP as a second line of defence.
func sanitizeEmailHTML(raw string) string {
	doc, err := xhtml.Parse(strings.NewReader(raw))
	if err != nil {
		return ""
	}
	var clean func(*xhtml.Node)
	clean = func(parent *xhtml.Node) {
		for node := parent.FirstChild; node != nil; {
			next := node.NextSibling
			if node.Type == xhtml.ElementNode {
				tag := strings.ToLower(node.Data)
				switch tag {
				case "script", "iframe", "frame", "frameset", "object", "embed", "applet", "base":
					parent.RemoveChild(node)
					node = next
					continue
				case "form":
					// Preserve visible layout/content but remove form semantics.
					node.Data = "div"
					node.Attr = nil
				case "button", "textarea", "select", "option":
					node.Data = "span"
					node.Attr = nil
				case "input":
					parent.RemoveChild(node)
					node = next
					continue
				case "meta":
					if hasRefreshDirective(node) {
						parent.RemoveChild(node)
						node = next
						continue
					}
				}
				node.Attr = sanitizeEmailAttributes(node.Attr)
				if tag == "style" {
					for child := node.FirstChild; child != nil; child = child.NextSibling {
						if child.Type == xhtml.TextNode {
							child.Data = unsafeCSSPattern.ReplaceAllString(child.Data, "blocked:")
						}
					}
				}
			}
			clean(node)
			node = next
		}
	}
	clean(doc)
	var output strings.Builder
	if err := xhtml.Render(&output, doc); err != nil {
		return ""
	}
	return output.String()
}

func hasRefreshDirective(node *xhtml.Node) bool {
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, "http-equiv") && strings.EqualFold(strings.TrimSpace(attr.Val), "refresh") {
			return true
		}
	}
	return false
}

func sanitizeEmailAttributes(attributes []xhtml.Attribute) []xhtml.Attribute {
	out := make([]xhtml.Attribute, 0, len(attributes))
	for _, attr := range attributes {
		name := strings.ToLower(strings.TrimSpace(attr.Key))
		value := strings.TrimSpace(attr.Val)
		if strings.HasPrefix(name, "on") || name == "srcdoc" || name == "target" || name == "ping" || strings.HasPrefix(name, "form") {
			continue
		}
		switch name {
		case "style":
			if unsafeCSSPattern.MatchString(value) {
				continue
			}
		case "href", "src", "poster", "background", "xlink:href":
			if !safeEmailURL(value, name) {
				continue
			}
		case "srcset":
			if unsafeCSSPattern.MatchString(value) || strings.Contains(strings.ToLower(value), "data:") || strings.ContainsAny(value, "\r\n") {
				continue
			}
		}
		out = append(out, attr)
	}
	return out
}

func safeEmailURL(value, attribute string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "#") || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../") {
		return true
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return true
	case "mailto":
		return attribute == "href"
	case "cid":
		return attribute == "src" || attribute == "background" || attribute == "poster"
	case "data":
		lower := strings.ToLower(value)
		return attribute == "src" && (strings.HasPrefix(lower, "data:image/png;") || strings.HasPrefix(lower, "data:image/jpeg;") || strings.HasPrefix(lower, "data:image/gif;") || strings.HasPrefix(lower, "data:image/webp;"))
	default:
		return parsed.Scheme == ""
	}
}

func (c *NeteaseWebClient) listMessages(limit, start int) ([]Message, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	payload := fmt.Sprintf(`<object><int name="fid">1</int><string name="order">date</string><boolean name="desc">true</boolean><int name="limit">%d</int><int name="start">%d</int><boolean name="skipLockedFolders">false</boolean><string name="topFlag">top</string><boolean name="returnTag">true</boolean><boolean name="returnTotal">true</boolean></object>`, limit, start)
	raw, err := c.call("mbox:listMessages", payload)
	if err != nil {
		return nil, err
	}
	root, err := parseNeteaseResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("163 邮件列表格式已变化，请重新授权并更新扩展")
	}
	var items []Message
	for _, obj := range namedObjects(root, "items") {
		message := messageFromNode(obj)
		if message.ID != "" {
			items = append(items, message)
		}
	}
	if len(items) == 0 {
		for _, obj := range allObjects(root) {
			message := messageFromNode(obj)
			if message.ID != "" && (message.Subject != "" || message.From != "") {
				items = append(items, message)
			}
		}
	}
	return dedupeMessages(items), nil
}

func (c *NeteaseWebClient) call(functionName, payload string) ([]byte, error) {
	endpoint := url.URL{Scheme: "https", Host: c.host, Path: "/js6/s"}
	query := endpoint.Query()
	query.Set("sid", c.sessionID)
	query.Set("func", functionName)
	endpoint.RawQuery = query.Encode()
	form := url.Values{"var": {payload}}
	req, err := http.NewRequest(http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "application/xml,text/xml,*/*;q=0.8")
	req.Header.Set("Origin", "https://mail.163.com")
	req.Header.Set("Referer", "https://mail.163.com/js6/main.jsp?sid="+url.QueryEscape(c.sessionID))
	req.Header.Set("User-Agent", "Mozilla/5.0 CYMail/1.0")
	req.Header.Set("Cookie", cookieHeader(c.cookies))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("访问 163 网页邮箱失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 163 网页响应失败: %w", err)
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || strings.Contains(contentType, "text/html") || bytes.Contains(bytes.ToLower(raw), []byte("login.jsp")) {
		return nil, fmt.Errorf("163 网页登录已失效，请重新网页登录授权")
	}
	upperBody := bytes.ToUpper(bytes.TrimSpace(raw))
	if bytes.Contains(upperBody, []byte("FA_SECURITY")) || bytes.Contains(upperBody, []byte("S_AUTH")) || bytes.Contains(upperBody, []byte("NOTLOGIN")) {
		return nil, fmt.Errorf("163 网页登录已失效，请重新网页登录授权")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("163 网页接口返回 HTTP %d", resp.StatusCode)
	}
	return raw, nil
}

type neteaseNode struct {
	XMLName xml.Name
	Name    string        `xml:"name,attr"`
	Text    string        `xml:",chardata"`
	Nodes   []neteaseNode `xml:",any"`
}

func parseNeteaseResponse(raw []byte) (*neteaseNode, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty response")
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		var value interface{}
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return nil, err
		}
		return jsonToNode("result", value), nil
	}
	var root neteaseNode
	if err := xml.Unmarshal(trimmed, &root); err != nil {
		return nil, err
	}
	return &root, nil
}

func jsonToNode(name string, value interface{}) *neteaseNode {
	node := &neteaseNode{Name: name}
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, child := range typed {
			node.Nodes = append(node.Nodes, *jsonToNode(key, child))
		}
	case []interface{}:
		for _, child := range typed {
			node.Nodes = append(node.Nodes, *jsonToNode("item", child))
		}
	case string:
		node.Text = typed
	case float64:
		node.Text = strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		node.Text = strconv.FormatBool(typed)
	}
	return node
}

func firstObject(node *neteaseNode) *neteaseNode {
	if node == nil {
		return nil
	}
	if node.XMLName.Local == "object" || node.Name == "item" {
		return node
	}
	for index := range node.Nodes {
		if found := firstObject(&node.Nodes[index]); found != nil {
			return found
		}
	}
	return nil
}

func allObjects(node *neteaseNode) []*neteaseNode {
	var out []*neteaseNode
	var walk func(*neteaseNode)
	walk = func(current *neteaseNode) {
		if current.XMLName.Local == "object" || current.Name == "item" {
			out = append(out, current)
		}
		for index := range current.Nodes {
			walk(&current.Nodes[index])
		}
	}
	if node != nil {
		walk(node)
	}
	return out
}

func namedObjects(node *neteaseNode, name string) []*neteaseNode {
	var out []*neteaseNode
	var walk func(*neteaseNode, bool)
	walk = func(current *neteaseNode, inside bool) {
		inside = inside || strings.EqualFold(current.Name, name)
		if inside && (current.XMLName.Local == "object" || current.Name == "item") {
			out = append(out, current)
			return
		}
		for index := range current.Nodes {
			walk(&current.Nodes[index], inside)
		}
	}
	if node != nil {
		walk(node, false)
	}
	return out
}

func messageFromNode(node *neteaseNode) Message {
	date := NormalizeMessageDate(firstNamedText(node, "sentDate", "receivedDate", "date", "time"))
	return Message{
		ID: firstNamedText(node, "id", "mid"), From: firstNamedText(node, "from", "sender"),
		To: firstNamedText(node, "to", "recipient", "deliveredTo"), Subject: firstNamedText(node, "subject"),
		Date: date, Preview: firstNamedText(node, "summary", "preview", "snippet", "content"),
	}
}

func firstNamedText(node *neteaseNode, names ...string) string {
	if node == nil {
		return ""
	}
	for _, name := range names {
		if strings.EqualFold(node.Name, name) || strings.EqualFold(node.XMLName.Local, name) {
			if value := strings.TrimSpace(flattenText(node)); value != "" {
				return value
			}
		}
	}
	for index := range node.Nodes {
		if value := firstNamedText(&node.Nodes[index], names...); value != "" {
			return value
		}
	}
	return ""
}

func flattenText(node *neteaseNode) string {
	parts := []string{strings.TrimSpace(node.Text)}
	for index := range node.Nodes {
		if value := strings.TrimSpace(flattenText(&node.Nodes[index])); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func recipientMatches(value string, wanted map[string]struct{}) []string {
	lower := strings.ToLower(value)
	var matches []string
	for recipient := range wanted {
		if strings.Contains(lower, recipient) {
			matches = append(matches, recipient)
		}
	}
	return matches
}

func filterByDays(messages []Message, days int) []Message {
	if days <= 0 {
		return messages
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	out := messages[:0]
	for _, message := range messages {
		parsed, ok := ParseMessageDate(message.Date)
		if !ok || !parsed.Before(cutoff) {
			out = append(out, message)
		}
	}
	return out
}

func dedupeMessages(messages []Message) []Message {
	seen := make(map[string]struct{}, len(messages))
	out := make([]Message, 0, len(messages))
	for _, message := range messages {
		if _, exists := seen[message.ID]; exists {
			continue
		}
		seen[message.ID] = struct{}{}
		out = append(out, message)
	}
	return out
}

func cookieHeader(cookies map[string]string) string {
	parts := make([]string, 0, len(cookies))
	for name, value := range cookies {
		if strings.TrimSpace(name) != "" && value != "" {
			parts = append(parts, name+"="+value)
		}
	}
	return strings.Join(parts, "; ")
}

func cloneCookies(cookies map[string]string) map[string]string {
	copy := make(map[string]string, len(cookies))
	for name, value := range cookies {
		copy[name] = value
	}
	return copy
}

func xmlEscape(value string) string {
	var output strings.Builder
	_ = xml.EscapeText(&output, []byte(value))
	return output.String()
}
