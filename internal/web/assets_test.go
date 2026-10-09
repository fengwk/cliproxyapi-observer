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

// 全局筛选必须合并为单一工具栏：一个 aria-label="筛选条件" 区域同时承载时间范围、
// Provider、模型、客户端指纹、上游凭据与统一的应用按钮；不得再出现独立的 Key 筛选卡片
// 或冗长的 Key 筛选说明段落。
func TestFilterToolbarIsSingleConsolidatedArea(t *testing.T) {
	html, _, _ := Asset("ui.html")
	page := string(html)

	if got := strings.Count(page, `aria-label="筛选条件"`); got != 1 {
		t.Fatalf("ui.html should expose exactly one 筛选条件 region, got %d", got)
	}
	if strings.Contains(page, `aria-label="Key 筛选"`) {
		t.Fatal("ui.html must not keep a separate Key 筛选 card")
	}
	if strings.Contains(page, "Key 筛选影响概览") {
		t.Fatal("ui.html must not keep the removed Key-filter help paragraph")
	}
	if strings.Contains(page, "request-filters") {
		t.Fatal("ui.html must not use the removed request-filters layout")
	}
	for _, id := range []string{
		`id="filter-provider"`,
		`id="filter-model"`,
		`id="filter-client-key"`,
		`id="filter-auth"`,
		`id="identity-apply"`,
		`id="range-label"`,
	} {
		if !strings.Contains(page, id) {
			t.Fatalf("ui.html missing filter control %s", id)
		}
	}
	if !strings.Contains(page, ">应用筛选<") {
		t.Fatal("ui.html apply button should read 应用筛选")
	}
}

// 客户端 key 筛选统一为原生下拉：只做选择、默认明确「全部」、含「未归属（含旧记录）」；
// 不得再保留需手填的 input 或 datalist 候选。
func TestClientKeyFilterIsNativeSelect(t *testing.T) {
	html, _, _ := Asset("ui.html")
	page := string(html)

	selectTag := regexp.MustCompile(`(?s)<select[^>]*\bid="filter-client-key"[^>]*>(.*?)</select>`).FindStringSubmatch(page)
	if selectTag == nil {
		t.Fatal("filter-client-key should be a native <select>")
	}
	if body := selectTag[1]; !strings.Contains(body, `<option value="">全部</option>`) ||
		!strings.Contains(body, `<option value="unknown">未归属（含旧记录）</option>`) {
		t.Fatalf("client key select should default to 全部 and include 未归属, got %q", body)
	}
	for _, needle := range []string{
		`list="client-key-options"`,
		`id="client-key-options"`,
		`<datalist`,
		`id="filter-client-key" type=`,
	} {
		if strings.Contains(page, needle) {
			t.Fatalf("ui.html must not keep removable client-key input/datalist marker %q", needle)
		}
	}
}

// 工具栏样式必须收敛到单一 .controls 行，移除废弃的 request-filters / toolbar-spacer，
// 并隐藏空的凭据状态占位；同时阈值文案不再重复「含缓存」，但保留严格大于语义。
func TestFilterToolbarStylesAndPricingCopyConsolidated(t *testing.T) {
	css, _, _ := Asset("ui.css")
	style := string(css)
	for _, needle := range []string{".toolbar .controls", "#credential-status:empty"} {
		if !strings.Contains(style, needle) {
			t.Fatalf("ui.css missing consolidated toolbar rule %q", needle)
		}
	}
	for _, needle := range []string{".toolbar-spacer", ".request-filters"} {
		if strings.Contains(style, needle) {
			t.Fatalf("ui.css should drop unused rule %q", needle)
		}
	}

	js, _, _ := Asset("ui.js")
	source := string(js)
	if strings.Contains(source, "含缓存") {
		t.Fatal("ui.js must not repeat 含缓存 in pricing UI copy")
	}
	if !strings.Contains(source, "输入 Token >（可选）") {
		t.Fatal("ui.js threshold label should read 输入 Token >（可选）")
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

// Native scrollbars and form controls must follow the selected document theme.
func TestCSSNativeColorSchemes(t *testing.T) {
	css, _, _ := Asset("ui.css")
	for _, tc := range []struct {
		selector string
		scheme   string
	}{
		{":root", "light"},
		{"[data-theme='white']", "light"},
		{"[data-theme='dark']", "dark"},
	} {
		t.Run(tc.selector, func(t *testing.T) {
			block := regexp.MustCompile(regexp.QuoteMeta(tc.selector) + `\s*\{([^}]+)\}`).FindStringSubmatch(string(css))
			if len(block) != 2 {
				t.Fatalf("missing theme selector %q", tc.selector)
			}
			property := regexp.MustCompile(`\bcolor-scheme\s*:\s*` + tc.scheme + `\s*;`)
			if !property.MatchString(block[1]) {
				t.Fatalf("%s must declare color-scheme: %s", tc.selector, tc.scheme)
			}
		})
	}
}
