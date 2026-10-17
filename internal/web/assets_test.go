package web

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// requireAsset keeps the asset tests standalone: they inspect only the embedded
// package data and never construct a plugin engine or management request.
func requireAsset(t *testing.T, name string) ([]byte, string) {
	t.Helper()
	body, contentType, ok := Asset(name)
	if !ok {
		t.Fatalf("Asset(%q) not found", name)
	}
	if len(body) == 0 {
		t.Fatalf("Asset(%q) is empty", name)
	}
	return body, contentType
}

// compactSource compares code semantics rather than formatting: whitespace and
// quote style are irrelevant after normalization, so prettier cannot cause
// false failures while identifiers, operators and literals remain checked.
func compactSource(source string) string {
	source = strings.NewReplacer(`"`, "", `'`, "").Replace(source)
	return strings.Join(strings.Fields(source), "")
}

func containsCode(source, snippet string) bool {
	return strings.Contains(compactSource(source), compactSource(snippet))
}

func TestAssetKnownNamesAndContentTypes(t *testing.T) {
	cases := map[string]string{
		"ui.html": ContentTypeHTML,
		"ui.css":  ContentTypeCSS,
		"ui.js":   ContentTypeJS,
	}
	for name, wantType := range cases {
		_, contentType := requireAsset(t, name)
		if contentType != wantType {
			t.Fatalf("Asset(%q) content type = %q, want %q", name, contentType, wantType)
		}
		// 名称首尾空白应被容忍，避免宿主拼接时的小误差。
		if _, _, ok := Asset("  " + name + " \n"); !ok {
			t.Fatalf("Asset should trim whitespace around %q", name)
		}
	}
}

func TestAssetUnknownName(t *testing.T) {
	for _, name := range []string{"", "ui.txt", "../ui.js", "ui.js/../assets.go", "assets.go", "settings.css", "settings.js", "models.js", "  models.js \n"} {
		if body, contentType, ok := Asset(name); ok || body != nil || contentType != "" {
			t.Fatalf("Asset(%q) = (%v, %q, %v), want not found", name, body, contentType, ok)
		}
	}
}

// Asset 必须返回副本：调用方修改不得污染二进制内嵌的原始字节。
func TestAssetReturnsIsolatedCopy(t *testing.T) {
	for _, name := range []string{"ui.html", "ui.css", "ui.js"} {
		body, _ := requireAsset(t, name)
		body[0] = 'X'
		again, _ := requireAsset(t, name)
		if again[0] == 'X' {
			t.Fatalf("Asset(%q) returned a mutable reference into the embedded data", name)
		}
	}
}

// 面板必须依赖零外部资源（无 CDN / 外部字体 / 外部脚本），保证 CSP 'self' 成立。
var externalURL = regexp.MustCompile(`(?i)https?://|//[a-z0-9.-]+\.[a-z]{2,}`)

func TestHTMLIsSelfContainedAndCSPCompatible(t *testing.T) {
	html, _ := requireAsset(t, "ui.html")
	page := string(html)

	if !containsCode(page, `lang="zh-CN"`) {
		t.Fatal("ui.html should declare lang=\"zh-CN\"")
	}
	if externalURL.MatchString(page) {
		t.Fatal("ui.html must not reference external resources")
	}
	if !containsCode(page, `href="/v0/resource/plugins/cliproxyapi-observer/ui.css"`) {
		t.Fatal("ui.html must load ui.css same-origin")
	}
	if !containsCode(page, `src="/v0/resource/plugins/cliproxyapi-observer/ui.js"`) {
		t.Fatal("ui.html must load the bundled ui.js same-origin")
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

	for _, directive := range []string{
		`default-src 'self'`,
		`script-src 'self'`,
		`style-src 'self'`,
		`connect-src 'self'`,
		`img-src 'self'`,
	} {
		if !containsCode(page, directive) {
			t.Fatalf("ui.html CSP missing %q", directive)
		}
	}
}

func TestJSAssetsAreTextSafeAndSelfContained(t *testing.T) {
	for _, file := range []struct {
		name string
		body []byte
	}{
		{"ui.js", uiJS},
		{"settings.js", settingsJS},
		{"models.js", modelsJS},
	} {
		source := string(file.body)
		for _, needle := range []string{
			"innerHTML",
			"outerHTML",
			"insertAdjacentHTML",
			"document.write",
			"eval(",
			"new Function",
			"window.open",
			"localStorage",
			"document.cookie",
		} {
			if strings.Contains(source, needle) {
				t.Fatalf("%s must not use %q", file.name, needle)
			}
		}
		// SVG 命名空间是常量而非网络资源，检查外链前先剥离。
		if externalURL.MatchString(strings.ReplaceAll(source, "http://www.w3.org/2000/svg", "")) {
			t.Fatalf("%s must not reference external resources", file.name)
		}
		if !strings.Contains(source, "textContent") {
			t.Fatalf("%s must render dynamic text with textContent", file.name)
		}
	}
}

// 登录只持有当前标签页会话密钥；发送时使用 Bearer，401 立即退出且不回显输入。
func TestAuthControlsUseSessionBearerOnly(t *testing.T) {
	html, _ := requireAsset(t, "ui.html")
	page := string(html)
	for _, needle := range []string{
		`id="login"`,
		`type="password"`,
		`autocomplete="off"`,
		`required`,
		`>登录</button>`,
	} {
		if !containsCode(page, needle) {
			t.Fatalf("ui.html missing auth control %q", needle)
		}
	}

	js, _ := requireAsset(t, "ui.js")
	source := string(js)
	for _, needle := range []string{
		"Authorization:'Bearer '+key",
		"sessionStorage.getItem('observer.management-key')",
		"sessionStorage.setItem('observer.management-key',key)",
		"sessionStorage.removeItem('observer.management-key')",
		"r.status===401",
		"login.querySelector('input').value=''",
	} {
		if !containsCode(source, needle) {
			t.Fatalf("ui.js missing auth guard %q", needle)
		}
	}
}

// 取消路径必须显式：对话框关闭有清理逻辑，破坏性操作与放弃草稿都先确认。
func TestCancelControlsAreExplicit(t *testing.T) {
	uiJS, _ := requireAsset(t, "ui.js")
	uiSource := string(uiJS)
	for _, needle := range []string{
		"d.addEventListener('cancel'",
		"d.addEventListener('close'",
		"d.replaceChildren()",
		"d.remove()",
		"function confirmed(",
		"button('退出',logout)",
	} {
		if !containsCode(uiSource, needle) {
			t.Fatalf("ui.js missing cancel/cleanup control %q", needle)
		}
	}

	settingsSource := string(settingsJS)
	for _, needle := range []string{
		"重新加载 / 放弃草稿",
		"win.confirm",
		"已取消压缩",
	} {
		if !containsCode(settingsSource, needle) {
			t.Fatalf("settings.js missing cancel control %q", needle)
		}
	}
}

// /ui.js 是固定端点：Asset 必须按 settings.js + "\n" + models.js + "\n" + ui.js
// 的顺序返回 bundle，且不额外暴露 settings.js / models.js 资源。
func TestUIAssetBundlesSettingsAndModelsBeforeUI(t *testing.T) {
	body, contentType := requireAsset(t, "ui.js")
	if contentType != ContentTypeJS {
		t.Fatalf("ui.js content type = %q, want %q", contentType, ContentTypeJS)
	}
	if len(modelsJS) == 0 {
		t.Fatal("models.js must be embedded in the binary")
	}
	want := make([]byte, 0, len(settingsJS)+len(modelsJS)+2+len(uiJS))
	want = append(want, settingsJS...)
	want = append(want, '\n')
	want = append(want, modelsJS...)
	want = append(want, '\n')
	want = append(want, uiJS...)
	if !bytes.Equal(body, want) {
		t.Fatal("ui.js asset must be settings.js + newline + models.js + newline + ui.js")
	}

	source := string(body)
	compact := compactSource(source)
	settingsAt := strings.Index(compact, compactSource("window.ObserverSettings = ObserverSettings"))
	modelsAt := strings.Index(compact, compactSource("window.ObserverModels = ObserverModels"))
	uiAt := strings.Index(compact, compactSource("window.ObserverUI = {"))
	if settingsAt < 0 || modelsAt < 0 || uiAt < 0 || !(settingsAt < modelsAt && modelsAt < uiAt) {
		t.Fatalf("ui.js bundle order invalid: settings at %d, models at %d, UI at %d", settingsAt, modelsAt, uiAt)
	}
	for _, name := range []string{"settings.js", "models.js"} {
		if _, _, ok := Asset(name); ok {
			t.Fatalf("%s must not be exposed as a separate asset endpoint", name)
		}
	}
}

// UI 只能调用管理 API 已注册的固定 query-ID 路由；旧动态路径不得残留，
// 否则宿主只会返回 404，且静默保留死兼容路由会误导后续维护。
func TestUIAssetUsesFixedQueryIDRoutes(t *testing.T) {
	body, _ := requireAsset(t, "ui.js")
	source := string(body)
	for _, route := range []string{
		`/requests/detail?id=" + encodeURIComponent(`,
		`/keys?id=" + encodeURIComponent(`,
		`/keys/rotate?id=" + encodeURIComponent(`,
		`/keys/reset?id=" + encodeURIComponent(`,
		`/keys/recover?id=" + encodeURIComponent(`,
	} {
		if !containsCode(source, route) {
			t.Fatalf("ui.js missing fixed query-ID route %q", route)
		}
	}
	for _, legacy := range []string{
		`/requests/" + encodeURIComponent(`,
		`/keys/" + k.id`,
		`/keys/" + encodeURIComponent(k.id) + "/rotate"`,
		`/keys/" + encodeURIComponent(k.id) + "/reset"`,
	} {
		if containsCode(source, legacy) {
			t.Fatalf("ui.js still builds the removed dynamic route %q", legacy)
		}
	}
}

func TestSettingsAssetContract(t *testing.T) {
	uiJS, _ := requireAsset(t, "ui.js")
	uiSource := string(uiJS)
	for _, needle := range []string{
		"window.ObserverSettings = ObserverSettings",
		"module.exports = ObserverSettings",
		"UI.micro",
		"window.ObserverUI",
		"window.ObserverSettings(",
	} {
		if !containsCode(uiSource, needle) {
			t.Fatalf("bundled ui.js missing shared contract %q", needle)
		}
	}
}

func TestCSSThemesAndNoExternalImports(t *testing.T) {
	css, _ := requireAsset(t, "ui.css")
	style := string(css)
	for _, needle := range []string{
		":root{",
		"[data-theme=white]",
		"[data-theme=dark]",
		"color-scheme:light",
		"color-scheme:dark",
		"--bg:#faf9f5",
		"--panel:#fff",
		"--error:#a43d32",
		"[hidden]",
	} {
		if !containsCode(style, needle) {
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
