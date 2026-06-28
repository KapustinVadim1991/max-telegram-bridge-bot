package main

import (
	"strings"
	"unicode/utf8"
)

// Лимиты Telegram (в UTF-16 code units, как их считает Telegram):
//   - подпись (caption) к медиа — 1024;
//   - текстовое сообщение — 4096.
// Считаем точно, поэтому используем сами лимиты без запаса.
const (
	tgCaptionLimit = 1024
	tgTextLimit    = 4096
)

// utf16Len возвращает длину строки в UTF-16 code units — именно так Telegram
// считает длину текста/подписи (символы вне BMP занимают 2 единицы).
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// splitPlainForTg режет обычный (не-HTML) текст на куски: первый ≤ firstLimit,
// остальные ≤ restLimit. Режет по возможности на границе перевода строки/пробела.
func splitPlainForTg(s string, firstLimit, restLimit int) []string {
	if utf16Len(s) <= firstLimit {
		return []string{s}
	}
	runes := []rune(s)
	var chunks []string
	limit := firstLimit
	i := 0
	for i < len(runes) {
		vis := 0
		lastSpace := -1
		j := i
		for j < len(runes) {
			w := 1
			if runes[j] > 0xFFFF {
				w = 2
			}
			if vis+w > limit && vis > 0 {
				break
			}
			vis += w
			if runes[j] == ' ' || runes[j] == '\n' {
				lastSpace = j
			}
			j++
		}
		end := j
		next := j
		if j < len(runes) && lastSpace > i {
			end = lastSpace
			next = lastSpace + 1
		}
		if chunk := strings.TrimSpace(string(runes[i:end])); chunk != "" {
			chunks = append(chunks, chunk)
		}
		i = next
		limit = restLimit
	}
	return chunks
}

// htmlAtom — единица разбора HTML: либо тег, либо видимый символ/HTML-entity.
type htmlAtom struct {
	raw      string
	width    int  // видимая ширина в UTF-16 units (для тегов 0)
	isTag    bool
	tagKind  int  // 1 — открывающий, 2 — закрывающий, 0 — self-closing/прочее
	closeRaw string // для открывающего тега — соответствующий закрывающий
	isSpace  bool
}

// tokenizeHTMLForTg разбивает HTML-строку (как её формирует maxMarkupsToHTML —
// теги b/i/code/s/u/a и entity от html.EscapeString) на атомы.
func tokenizeHTMLForTg(s string) []htmlAtom {
	var atoms []htmlAtom
	n := len(s)
	i := 0
	for i < n {
		c := s[i]
		if c == '<' {
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				atoms = append(atoms, htmlAtom{raw: "<", width: 1})
				i++
				continue
			}
			tagStr := s[i : i+end+1]
			a := htmlAtom{raw: tagStr, isTag: true}
			inner := strings.TrimSpace(tagStr[1 : len(tagStr)-1])
			switch {
			case strings.HasSuffix(inner, "/"):
				a.tagKind = 0
			case strings.HasPrefix(inner, "/"):
				a.tagKind = 2
			default:
				a.tagKind = 1
				name := inner
				if sp := strings.IndexAny(name, " \t\n"); sp >= 0 {
					name = name[:sp]
				}
				a.closeRaw = "</" + name + ">"
			}
			atoms = append(atoms, a)
			i += end + 1
			continue
		}
		if c == '&' {
			lim := i + 12
			if lim > n {
				lim = n
			}
			if semi := strings.IndexByte(s[i:lim], ';'); semi > 0 {
				atoms = append(atoms, htmlAtom{raw: s[i : i+semi+1], width: 1})
				i += semi + 1
				continue
			}
			atoms = append(atoms, htmlAtom{raw: "&", width: 1})
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		atoms = append(atoms, htmlAtom{raw: s[i : i+size], width: w, isSpace: r == ' ' || r == '\n'})
		i += size
	}
	return atoms
}

// computeRangesHTML определяет границы кусков [start,end) по видимой длине,
// предпочитая разрыв на пробеле/переводе строки. Первый кусок ≤ firstLimit,
// остальные ≤ restLimit. Теги в длину не считаются.
func computeRangesHTML(atoms []htmlAtom, firstLimit, restLimit int) [][2]int {
	var ranges [][2]int
	n := len(atoms)
	start := 0
	curVis := 0
	lastSpace := -1
	limit := firstLimit
	for j := 0; j < n; j++ {
		a := atoms[j]
		if a.isTag {
			continue
		}
		if curVis+a.width > limit && curVis > 0 {
			end := j
			next := j
			if lastSpace > start {
				end = lastSpace
				next = lastSpace + 1
			}
			ranges = append(ranges, [2]int{start, end})
			start = next
			limit = restLimit
			curVis = 0
			lastSpace = -1
			for k := start; k <= j; k++ {
				if !atoms[k].isTag {
					curVis += atoms[k].width
					if atoms[k].isSpace {
						lastSpace = k
					}
				}
			}
			continue
		}
		curVis += a.width
		if a.isSpace {
			lastSpace = j
		}
	}
	ranges = append(ranges, [2]int{start, n})
	return ranges
}

// renderRangeHTML собирает кусок [start,end), переоткрывая теги, унаследованные
// от предыдущего куска, и закрывая все теги, оставшиеся открытыми в конце.
// Возвращает строку куска и список тегов, открытых на момент его конца
// (наследуется следующим куском).
func renderRangeHTML(atoms []htmlAtom, start, end int, inherited []htmlAtom) (string, []htmlAtom) {
	var sb strings.Builder
	open := append([]htmlAtom{}, inherited...)
	for _, t := range inherited {
		sb.WriteString(t.raw)
	}
	for k := start; k < end; k++ {
		a := atoms[k]
		sb.WriteString(a.raw)
		if a.isTag {
			switch a.tagKind {
			case 1:
				open = append(open, a)
			case 2:
				if len(open) > 0 {
					open = open[:len(open)-1]
				}
			}
		}
	}
	for i := len(open) - 1; i >= 0; i-- {
		sb.WriteString(open[i].closeRaw)
	}
	return sb.String(), open
}

// splitHTMLForTg режет HTML-форматированный текст на куски, не ломая теги:
// первый ≤ firstLimit, остальные ≤ restLimit (видимая длина в UTF-16 units).
// Открытые теги корректно закрываются в конце куска и переоткрываются в начале
// следующего.
func splitHTMLForTg(s string, firstLimit, restLimit int) []string {
	atoms := tokenizeHTMLForTg(s)
	total := 0
	for _, a := range atoms {
		total += a.width
	}
	if total <= firstLimit {
		return []string{s}
	}
	ranges := computeRangesHTML(atoms, firstLimit, restLimit)
	var chunks []string
	var inherited []htmlAtom
	for _, rg := range ranges {
		var chunk string
		chunk, inherited = renderRangeHTML(atoms, rg[0], rg[1], inherited)
		// Пропускаем куски без видимого содержимого (например, из одних тегов).
		if htmlVisibleWidth(chunk) > 0 {
			chunks = append(chunks, chunk)
		}
	}
	return chunks
}

// htmlVisibleWidth — видимая длина HTML-строки в UTF-16 units (без тегов).
func htmlVisibleWidth(s string) int {
	w := 0
	for _, a := range tokenizeHTMLForTg(s) {
		w += a.width
	}
	return w
}
