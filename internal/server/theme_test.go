package server

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/web"
)

func luminance(t *testing.T, hex string) float64 {
	t.Helper()
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		t.Fatalf("colour %q is not #rrggbb", hex)
	}
	var c [3]float64
	for i := range c {
		n, err := strconv.ParseUint(hex[2*i:2*i+2], 16, 8)
		if err != nil {
			t.Fatal(err)
		}
		v := float64(n) / 255
		if v <= 0.03928 {
			c[i] = v / 12.92
		} else {
			c[i] = math.Pow((v+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
}

func contrast(t *testing.T, a, b string) float64 {
	la, lb := luminance(t, a), luminance(t, b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// The UI is one theme defined by the tokens at the top of layout.html. Every
// text/background pair the stylesheet uses must stay readable (WCAG AA, 4.5:1),
// and no colour may be written anywhere but in that token block.
func TestThemeTokens(t *testing.T) {
	src, err := web.FS.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	css := string(src)
	root := css[strings.Index(css, ":root {"):]
	root = root[:strings.Index(root, "}")]
	tok := map[string]string{}
	for _, m := range regexp.MustCompile(`--([a-z-]+):\s*(#[0-9a-fA-F]{6})`).FindAllStringSubmatch(root, -1) {
		tok[m[1]] = m[2]
	}
	for _, name := range []string{"bg", "card", "fg", "muted", "line", "accent", "on-accent", "ok", "bad", "warn", "notice"} {
		if tok[name] == "" {
			t.Fatalf("theme token --%s is missing", name)
		}
	}
	for _, p := range [][2]string{
		{"fg", "bg"}, {"fg", "card"}, {"fg", "notice"},
		{"muted", "bg"}, {"muted", "card"},
		{"accent", "bg"}, {"accent", "card"},
		{"on-accent", "accent"}, {"on-accent", "bad"},
		{"ok", "card"}, {"bad", "bg"}, {"bad", "card"}, {"warn", "bg"}, {"warn", "card"},
	} {
		if r := contrast(t, tok[p[0]], tok[p[1]]); r < 4.5 {
			t.Errorf("--%s on --%s has contrast %.2f, want at least 4.5", p[0], p[1], r)
		}
	}
	if strings.Contains(css, "prefers-color-scheme") {
		t.Error("the UI has one theme; there is no light/dark switch")
	}

	// Colours belong in the token block only.
	hex := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	entries, err := web.FS.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, _ := web.FS.ReadFile("templates/" + e.Name())
		text := string(b)
		if e.Name() == "layout.html" {
			text = strings.Replace(text, root, "", 1)
		}
		// href="#..." style fragments are not colours; look only inside CSS-ish contexts.
		for _, m := range hex.FindAllString(text, -1) {
			if i := strings.Index(text, m); i > 0 && strings.ContainsAny(text[i-1:i], "\"'/=") {
				continue
			}
			t.Errorf("%s hardcodes the colour %s; use a theme token", e.Name(), m)
		}
	}
}
