package mail

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestHTMLToTextKeepsCompleteReadableBody(t *testing.T) {
	raw := `<html><head><style>.hidden{display:none}</style><script>secret()</script></head><body><div id="content"><p>Hello <strong>CYMail</strong></p><p>验证码：482911</p><blockquote>完整引用内容</blockquote></div></body></html>`
	got := htmlToText(raw)
	for _, expected := range []string{"Hello CYMail", "验证码：482911", "完整引用内容"} {
		if !strings.Contains(got, expected) {
			t.Fatalf("htmlToText missing %q: %q", expected, got)
		}
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "display:none") {
		t.Fatalf("htmlToText exposed script/style content: %q", got)
	}
}

func TestRewriteEmailImagesResolvesAndEmbedsProviderReferences(t *testing.T) {
	base, _ := url.Parse("https://mail.163.com/js6/read/readhtml.jsp?mid=123")
	raw := `<html><body><img src="../readdata.jsp?part=1"><img background="/static/bg.png"><img src="https://cdn.example/logo.png"></body></html>`
	got := rewriteEmailImages(raw, base, func(imageURL *url.URL) (string, bool) {
		if imageURL.Hostname() == "mail.163.com" && strings.Contains(imageURL.Path, "readdata.jsp") {
			return "data:image/png;base64,iVBORw0KGgo=", true
		}
		return "", false
	})
	for _, expected := range []string{
		`src="data:image/png;base64,iVBORw0KGgo="`,
		`background="https://mail.163.com/static/bg.png"`,
		`src="https://cdn.example/logo.png"`,
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("rewritten HTML missing %q: %s", expected, got)
		}
	}
}

func TestParseMessageDateSupportsProviderTimestampShapes(t *testing.T) {
	want := time.Date(2026, 8, 9, 4, 5, 6, 0, time.UTC)
	values := []string{
		"1786248306",
		"1786248306000",
		"1786248306000000",
		"1786248306000000000",
		"2026-08-09T12:05:06+08:00",
		"2026-08-09T12:05:06",
		"2026-08-09 12:05:06",
		"2026-08-09 12:05:06 CST",
		"Sun, 09 Aug 2026 12:05:06 +0800",
	}
	for _, value := range values {
		got, ok := ParseMessageDate(value)
		if !ok || !got.Equal(want) {
			t.Fatalf("ParseMessageDate(%q) = %s, %v; want %s", value, got, ok, want)
		}
	}
	if _, ok := ParseMessageDate("not-a-date"); ok {
		t.Fatal("invalid provider date was accepted")
	}
}

func TestSanitizeEmailHTMLPreservesPresentationAndImages(t *testing.T) {
	raw := `<!doctype html><html><head><style>.hero{color:#123;background:url(https://cdn.example/image.png)}</style></head><body onload="steal()"><form action="https://bad.example"><div class="hero" style="font-weight:bold">Hello <img src="https://cdn.example/logo.png" alt="Logo" width="120"></div></form><script>steal()</script></body></html>`
	got := sanitizeEmailHTML(raw)
	for _, expected := range []string{".hero{color:#123", `style="font-weight:bold"`, `src="https://cdn.example/logo.png"`, `alt="Logo"`, "Hello"} {
		if !strings.Contains(got, expected) {
			t.Fatalf("sanitized HTML missing %q: %s", expected, got)
		}
	}
	for _, forbidden := range []string{"<script", "steal()", "<form", "action=", "onload="} {
		if strings.Contains(strings.ToLower(got), strings.ToLower(forbidden)) {
			t.Fatalf("sanitized HTML retained %q: %s", forbidden, got)
		}
	}
}

func TestSanitizeEmailHTMLRejectsDangerousURLsAndActiveContent(t *testing.T) {
	raw := `<html><head><meta http-equiv="refresh" content="0;url=https://bad.example"></head><body><a href="javascript:alert(1)" target="_top">bad</a><img src="data:text/html,&lt;script&gt;alert(1)&lt;/script&gt;"><iframe src="https://bad.example"></iframe><p style="background:expression(alert(1))">safe text</p></body></html>`
	got := strings.ToLower(sanitizeEmailHTML(raw))
	for _, forbidden := range []string{"http-equiv=\"refresh\"", "javascript:", "data:text/html", "<iframe", "target=", "expression("} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("sanitized HTML retained %q: %s", forbidden, got)
		}
	}
	if !strings.Contains(got, "safe text") || !strings.Contains(got, ">bad</a>") {
		t.Fatalf("sanitizer removed visible fallback text: %s", got)
	}
}
