package main

import (
	"strings"
	"testing"
)

// visibleText возвращает «видимый» текст HTML-строки: убирает теги и декодирует
// базовые entity, которые порождает html.EscapeString.
func visibleText(s string) string {
	var sb strings.Builder
	for _, a := range tokenizeHTMLForTg(s) {
		if a.isTag {
			continue
		}
		switch a.raw {
		case "&amp;":
			sb.WriteString("&")
		case "&lt;":
			sb.WriteString("<")
		case "&gt;":
			sb.WriteString(">")
		case "&#34;", "&quot;":
			sb.WriteString("\"")
		case "&#39;":
			sb.WriteString("'")
		default:
			sb.WriteString(a.raw)
		}
	}
	return sb.String()
}

// tagsBalanced грубо проверяет, что в каждом куске открывающие/закрывающие теги
// сбалансированы (по LIFO-стеку имён).
func tagsBalanced(s string) bool {
	var stack []string
	for _, a := range tokenizeHTMLForTg(s) {
		if !a.isTag {
			continue
		}
		switch a.tagKind {
		case 1:
			name := strings.TrimSpace(a.raw[1 : len(a.raw)-1])
			if sp := strings.IndexAny(name, " \t\n"); sp >= 0 {
				name = name[:sp]
			}
			stack = append(stack, name)
		case 2:
			if len(stack) == 0 {
				return false
			}
			stack = stack[:len(stack)-1]
		}
	}
	return len(stack) == 0
}

func TestSplitPlainForTg(t *testing.T) {
	// Короткий текст не режется.
	if got := splitPlainForTg("hello", 100, 100); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("short text split: %v", got)
	}

	long := strings.Repeat("a", 50) + " " + strings.Repeat("b", 50)
	chunks := splitPlainForTg(long, 60, 60)
	if len(chunks) < 2 {
		t.Fatalf("expected >=2 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if utf16Len(c) > 60 {
			t.Errorf("chunk %d too long: %d", i, utf16Len(c))
		}
	}
	if joined := strings.ReplaceAll(strings.Join(chunks, ""), " ", ""); joined != strings.ReplaceAll(long, " ", "") {
		t.Errorf("content mismatch: %q", joined)
	}
}

func TestSplitHTMLForTgKeepsTagsBalancedAndContent(t *testing.T) {
	// Жирный спан тянется через границу разреза.
	in := "<b>" + strings.Repeat("word ", 60) + "</b>"
	chunks := splitHTMLForTg(in, 50, 50)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	var vis strings.Builder
	for i, c := range chunks {
		if w := htmlVisibleWidth(c); w > 50 {
			t.Errorf("chunk %d visible width %d > 50", i, w)
		}
		if !tagsBalanced(c) {
			t.Errorf("chunk %d not balanced: %q", i, c)
		}
		vis.WriteString(visibleText(c))
	}
	// Видимый текст склеенных кусков == видимому тексту оригинала (с точностью
	// до съеденных на границах пробелов).
	wantVis := strings.Join(strings.Fields(visibleText(in)), "")
	gotVis := strings.Join(strings.Fields(vis.String()), "")
	if wantVis != gotVis {
		t.Errorf("visible mismatch:\n want %q\n got  %q", wantVis, gotVis)
	}
}

func TestSplitHTMLForTgEntities(t *testing.T) {
	// Entity считается как 1 видимый символ и не должна рваться пополам.
	in := "<b>" + strings.Repeat("&lt;", 40) + "</b>"
	chunks := splitHTMLForTg(in, 20, 20)
	for i, c := range chunks {
		if w := htmlVisibleWidth(c); w > 20 {
			t.Errorf("chunk %d width %d > 20", i, w)
		}
		if strings.Count(c, "&") != strings.Count(c, ";") {
			t.Errorf("chunk %d has broken entity: %q", i, c)
		}
		if !tagsBalanced(c) {
			t.Errorf("chunk %d not balanced: %q", i, c)
		}
	}
}

func TestSplitHTMLForTgShortNoop(t *testing.T) {
	in := "<b>Имя</b>: привет"
	got := splitHTMLForTg(in, 1024, 4096)
	if len(got) != 1 || got[0] != in {
		t.Fatalf("short html should be unchanged: %v", got)
	}
}
