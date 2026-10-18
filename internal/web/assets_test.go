package web

import (
	"regexp"
	"strings"
	"testing"
)

// requireAsset retrieves an embedded asset by name and fails the test if missing or empty.
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

func containsCode(source, snippet string) bool {
	cleanSource := strings.Join(strings.Fields(strings.NewReplacer(`"`, "", `'`, "").Replace(source)), "")
	cleanSnippet := strings.Join(strings.Fields(strings.NewReplacer(`"`, "", `'`, "").Replace(snippet)), "")
	return strings.Contains(cleanSource, cleanSnippet)
}

func TestAssetKnownNamesAndContentTypes(t *testing.T) {
	cases := map[string]string{
		"ui.html":  ContentTypeHTML,
		"ui.css":   ContentTypeCSS,
		"ui.js":    ContentTypeJS,
		"theme.js": ContentTypeJS,
	}
	for name, wantType := range cases {
		_, contentType := requireAsset(t, name)
		if contentType != wantType {
			t.Fatalf("Asset(%q) content type = %q, want %q", name, contentType, wantType)
		}
		// Tolerates surrounding whitespace
		if _, _, ok := Asset("  " + name + " \n"); !ok {
			t.Fatalf("Asset should trim whitespace around %q", name)
		}
	}
}

func TestAssetUnknownName(t *testing.T) {
	for _, name := range []string{
		"",
		"ui.txt",
		"../ui.js",
		"ui.js/../assets.go",
		"assets.go",
		"settings.css",
		"settings.js",
		"models.js",
		"dist/ui.html",
		"index.html",
	} {
		if body, contentType, ok := Asset(name); ok || body != nil || contentType != "" {
			t.Fatalf("Asset(%q) = (%v, %q, %v), want not found", name, body, contentType, ok)
		}
	}
}

// Asset must return an isolated copy so mutation does not alter binary-embedded data.
func TestAssetReturnsIsolatedCopy(t *testing.T) {
	for _, name := range []string{"ui.html", "ui.css", "ui.js", "theme.js"} {
		body, _ := requireAsset(t, name)
		orig := body[0]
		body[0] = 'X'
		again, _ := requireAsset(t, name)
		if again[0] == 'X' && orig != 'X' {
			t.Fatalf("Asset(%q) returned a mutable reference into the embedded data", name)
		}
	}
}

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
	if !containsCode(page, `theme.js`) {
		t.Fatal("ui.html must load theme.js")
	}
	if !containsCode(page, `ui.css`) {
		t.Fatal("ui.html must load ui.css")
	}
	if !containsCode(page, `ui.js`) {
		t.Fatal("ui.html must load ui.js")
	}

	// No inline script tags without src
	for _, tag := range regexp.MustCompile(`(?i)<script\b[^>]*>`).FindAllString(page, -1) {
		if !strings.Contains(strings.ToLower(tag), "src=") {
			t.Fatalf("ui.html must not contain inline scripts: %q", tag)
		}
	}

	// No inline event handlers
	if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).MatchString(page) {
		t.Fatal("ui.html must not contain inline event handlers")
	}

	// No inline style attributes
	if regexp.MustCompile(`(?i)\sstyle\s*=`).MatchString(page) {
		t.Fatal("ui.html must not contain inline style attributes (CSP style-src 'self')")
	}

	for _, directive := range []string{
		`default-src 'self'`,
		`script-src 'self'`,
		`style-src 'self'`,
		`connect-src 'self'`,
		`img-src 'self'`,
		`base-uri 'none'`,
		`form-action 'none'`,
	} {
		if !containsCode(page, directive) {
			t.Fatalf("ui.html CSP missing %q", directive)
		}
	}
}

func TestStaticAssetsAreSelfContainedAndNoExternalURLs(t *testing.T) {
	for _, name := range []string{"ui.js", "ui.css", "theme.js"} {
		body, _ := requireAsset(t, name)
		source := string(body)

		// Standard W3C namespaces and React production invariant error decoder URLs are constant strings in React DOM
		cleanSource := source
		for _, benign := range []string{
			"http://www.w3.org/2000/svg",
			"http://www.w3.org/1999/xhtml",
			"http://www.w3.org/1999/xlink",
			"http://www.w3.org/1998/Math/MathML",
			"http://www.w3.org/XML/1998/namespace",
			"https://reactjs.org/docs/error-decoder.html",
			"https://react.dev/errors",
		} {
			cleanSource = strings.ReplaceAll(cleanSource, benign, "")
		}

		if externalURL.MatchString(cleanSource) {
			t.Fatalf("%s must not reference external web resources", name)
		}

		if name == "theme.js" {
			// theme.js must never touch document.cookie or localStorage
			if strings.Contains(source, "document.cookie") {
				t.Fatalf("theme.js must not access document.cookie")
			}
			if strings.Contains(source, "localStorage") {
				t.Fatalf("theme.js must not access localStorage")
			}
		}
	}
}
