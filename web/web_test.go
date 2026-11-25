package web

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEmbeddedAssets(t *testing.T) {
	for _, name := range []string{
		"index.html", "styles.css", "theme-init.js", "favicon.svg",
		"app.js", "api.js", "board.js", "drawer.js", "icons.js", "markdown.js", "modals.js", "models.js", "state.js", "ui.js", "util.js",
	} {
		data, err := fs.ReadFile(Files, name)
		if err != nil {
			t.Errorf("%s is not embedded: %v", name, err)
		} else if len(data) == 0 {
			t.Errorf("%s is empty", name)
		}
	}

	index, err := fs.ReadFile(Files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(index)
	// The page is served with a CSP that only allows same-origin scripts and
	// styles, so it must not rely on inline code or external hosts.
	for name, re := range map[string]*regexp.Regexp{
		"inline script":        regexp.MustCompile(`(?is)<script(?:\s[^>]*)?>\s*[^<\s]`),
		"inline style element": regexp.MustCompile(`(?i)<style[\s>]`),
		"inline style attr":    regexp.MustCompile(`(?i)\sstyle\s*=`),
		"inline event handler": regexp.MustCompile(`(?i)\son[a-z]+\s*=`),
		"external resource":    regexp.MustCompile(`(?i)(?:src|href)\s*=\s*["']?(?:https?:)?//`),
	} {
		if loc := re.FindStringIndex(html); loc != nil {
			t.Errorf("index.html contains an %s: %q", name, html[loc[0]:min(len(html), loc[1]+40)])
		}
	}

	// Every module a script imports must be embedded too.
	importRe := regexp.MustCompile(`from\s+'\./([^']+)'`)
	entries, _ := fs.ReadDir(Files, ".")
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		data, _ := fs.ReadFile(Files, e.Name())
		for _, m := range importRe.FindAllStringSubmatch(string(data), -1) {
			if _, err := fs.Stat(Files, m[1]); err != nil {
				t.Errorf("%s imports %s, which is not embedded", e.Name(), m[1])
			}
		}
	}
}

// isEmoji reports pictographic symbols and the modifiers that build emoji.
func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF,
		r >= 0x2600 && r <= 0x27BF,
		r >= 0x2B00 && r <= 0x2BFF,
		r >= 0x2300 && r <= 0x23FF,
		r >= 0xFE00 && r <= 0xFE0F,
		r >= 0xE0020 && r <= 0xE007F,
		r == 0x200D, r == 0x20E3, r == 0x00A9, r == 0x00AE, r == 0x203C, r == 0x2049, r == 0x2122, r == 0x2139,
		r >= 0x2194 && r <= 0x21AA, r == 0x24C2, r >= 0x25AA && r <= 0x25FE, r >= 0x2934 && r <= 0x2935,
		r == 0x3030, r == 0x303D, r == 0x3297, r == 0x3299:
		return true
	}
	return false
}

// TestNoEmoji guards the product rule that the UI, its copy, the code and
// the documentation contain no emoji. The shipped front-end is held to
// plain ASCII; user content is the only source of other characters.
func TestNoEmoji(t *testing.T) {
	textExt := map[string]bool{
		".go": true, ".js": true, ".mjs": true, ".css": true, ".html": true, ".svg": true,
		".md": true, ".sh": true, ".json": true, ".mod": true, ".service": true, ".txt": true,
	}
	root := ".."
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "data" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !textExt[filepath.Ext(path)] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		checked++
		if !utf8.Valid(data) {
			t.Errorf("%s is not valid UTF-8", path)
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		asciiOnly := strings.HasPrefix(rel, "web"+string(filepath.Separator)) || rel == "README.md" || rel == "run.sh"
		line := 1
		for _, r := range string(data) {
			if r == '\n' {
				line++
			}
			if isEmoji(r) {
				t.Errorf("%s:%d contains the emoji or pictograph %U", rel, line, r)
			} else if asciiOnly && r > 0x7E {
				t.Errorf("%s:%d contains the non-ASCII character %U", rel, line, r)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 25 {
		t.Fatalf("only %d files were checked; the walk did not start at the repository root", checked)
	}
}
