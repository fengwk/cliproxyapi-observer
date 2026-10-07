package web

import (
	"regexp"
	"strings"
	"testing"
)

func TestAssetKnownNamesAndContentTypes(t *testing.T) {
	cases := map[string]string{
		"ui.html": ContentTypeHTML,
		"ui.css":  ContentTypeCSS,
		"ui.js":   ContentTypeJS,
	}
	for name, wantType := range cases {
		body, contentType, ok := Asset(name)
		if !ok {
			t.Fatalf("Asset(%q) not found", name)
		}
		if contentType != wantType {
			t.Fatalf("Asset(%q) content type = %q, want %q", name, contentType, wantType)
		}
		if len(body) == 0 {
			t.Fatalf("Asset(%q) is empty", name)
		}
	}

	// 名称首尾空白应被容忍，避免宿主拼接时的小误差。
	if _, _, ok := Asset("  ui.js \n"); !ok {
		t.Fatal("Asset should trim whitespace around the name")
	}
}

func TestAssetUnknownName(t *testing.T) {
	for _, name := range []string{"", "ui.txt", "../ui.js", "ui.js/../assets.go", "assets.go"} {
		if body, contentType, ok := Asset(name); ok || body != nil || contentType != "" {
			t.Fatalf("Asset(%q) = (%v, %q, %v), want not found", name, body, contentType, ok)
		}
	}
}

// Asset 必须返回副本：调用方修改不得污染二进制内嵌的原始字节。
func TestAssetReturnsIsolatedCopy(t *testing.T) {
	body, _, ok := Asset("ui.js")
	if !ok {
		t.Fatal("ui.js missing")
	}
	if len(body) == 0 {
		t.Fatal("ui.js empty")
	}
	body[0] = 'X'
	again, _, _ := Asset("ui.js")
	if again[0] == 'X' {
		t.Fatal("Asset returned a mutable reference into the embedded data")
	}
}

// 面板必须依赖零外部资源（无 CDN / 外部字体 / 外部脚本），保证 CSP 'self' 成立。
var externalURL = regexp.MustCompile(`(?i)https?://|//[a-z0-9.-]+\.[a-z]{2,}`)

func TestHTMLIsSelfContainedAndCSPCompatible(t *testing.T) {
	html, _, _ := Asset("ui.html")
	page := string(html)

	if !strings.Contains(page, `lang="zh-CN"`) {
		t.Fatal("ui.html should declare lang=\"zh-CN\"")
	}
	if externalURL.MatchString(page) {
		t.Fatal("ui.html must not reference external resources")
	}
	if strings.Contains(page, `src="ui.js"`) == false {
		t.Fatal("ui.html must load ui.js same-origin")
	}
	if strings.Contains(page, `href="ui.css"`) == false {
		t.Fatal("ui.html must load ui.css same-origin")
	}
	// 内联脚本或事件处理器会削弱 CSP，禁止出现。
	for _, tag := range regexp.MustCompile(`(?i)<script\b[^>]*>`).FindAllString(page, -1) {
		if !strings.Contains(strings.ToLower(tag), "src=") {
			t.Fatalf("ui.html must not contain inline scripts: %q", tag)
		}
	}
	if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).MatchString(page) {
		t.Fatal("ui.html must not contain inline event handlers")
	}
	if regexp.MustCompile(`(?i)\sstyle\s*=`).MatchString(page) {
		t.Fatal("ui.html must not contain inline style attributes (CSP style-src 'self')")
	}
	if !strings.Contains(page, `default-src 'self'`) {
		t.Fatal("ui.html should declare a same-origin CSP meta policy")
	}
}

func TestJSForbiddenAPIsAbsent(t *testing.T) {
	js, _, _ := Asset("ui.js")
	source := string(js)
	for _, needle := range []string{
		"innerHTML",
		"outerHTML",
		"insertAdjacentHTML",
		"document.write",
		"eval(",
		"new Function",
		"localStorage",
		"sessionStorage",
		"document.cookie",
	} {
		if strings.Contains(source, needle) {
			t.Fatalf("ui.js must not use %q", needle)
		}
	}
	if externalURL.MatchString(source) {
		t.Fatal("ui.js must not reference external resources")
	}
}

func TestCSSThemesAndNoExternalImports(t *testing.T) {
	css, _, _ := Asset("ui.css")
	style := string(css)
	for _, needle := range []string{
		":root {",
		"[data-theme='white']",
		"[data-theme='dark']",
		"--bg-secondary: #faf9f5",
		"--radius-md: 8px",
	} {
		if !strings.Contains(style, needle) {
			t.Fatalf("ui.css missing theme marker %q", needle)
		}
	}
	if strings.Contains(style, "@import") {
		t.Fatal("ui.css must not use @import")
	}
	if externalURL.MatchString(style) {
		t.Fatal("ui.css must not reference external resources")
	}
}
