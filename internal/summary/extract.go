// Package summary produces short card summaries: a free extraction from the
// latest conversation turns, and an on-demand AI summary through the local
// copilot or codex CLI.
package summary

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Turn is one conversation message.
type Turn struct {
	Role string
	Text string
}

// AutoMaxLen is the target length of an automatically extracted summary.
const AutoMaxLen = 180

var (
	fencedCodeRe = regexp.MustCompile("(?s)```.*?(?:```|$)")
	imageRe      = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	linkRe       = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	htmlTagRe    = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)
	tableRuleRe  = regexp.MustCompile(`^[\s|:-]+$`)
	linePrefixRe = regexp.MustCompile(`^\s*(?:#{1,6}\s+|>\s?|[-*+]\s+|\d+[.)]\s+)+`)
	boldRe       = regexp.MustCompile(`(\*\*|__)(.+?)(\*\*|__)`)
	spaceRe      = regexp.MustCompile(`\s+`)
)

// isEmoji reports whether r is a pictographic symbol or an emoji modifier.
// Summaries shown on the board are kept free of them.
func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF, // pictographs, emoticons, transport, supplemental symbols
		r >= 0x2600 && r <= 0x27BF,   // miscellaneous symbols and dingbats
		r >= 0x2B00 && r <= 0x2BFF,   // stars and arrows used as emoji
		r >= 0x2300 && r <= 0x23FF,   // technical symbols (watch, hourglass, media keys)
		r >= 0xFE00 && r <= 0xFE0F,   // variation selectors
		r >= 0xE0020 && r <= 0xE007F, // tag characters
		r == 0x200D, r == 0x20E3, r == 0x2B50, r == 0x3030, r == 0x303D, r == 0x3297, r == 0x3299:
		return true
	}
	return false
}

// StripEmoji removes emoji code points from s.
func StripEmoji(s string) string {
	if !strings.ContainsFunc(s, isEmoji) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isEmoji(r) {
			return -1
		}
		return r
	}, s)
}

// Plain removes markdown structure, emoji and redundant whitespace, yielding
// a single line of text.
func Plain(text string) string {
	text = fencedCodeRe.ReplaceAllString(text, " ")
	text = imageRe.ReplaceAllString(text, "$1")
	text = linkRe.ReplaceAllString(text, "$1")
	text = htmlTagRe.ReplaceAllString(text, " ")
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.Contains(line, "|") {
			if tableRuleRe.MatchString(line) {
				continue
			}
			line = strings.ReplaceAll(line, "|", " ")
		}
		line = linePrefixRe.ReplaceAllString(line, "")
		kept = append(kept, line)
	}
	text = strings.Join(kept, " ")
	text = boldRe.ReplaceAllString(text, "$2")
	text = strings.ReplaceAll(text, "`", "")
	text = StripEmoji(text)
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	return strings.TrimSpace(spaceRe.ReplaceAllString(text, " "))
}

// Truncate shortens s to at most maxLen runes, preferring to cut at a
// sentence end or a word boundary, and marks the cut with "...".
func Truncate(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	runes := []rune(s)
	budget := maxLen - 3
	if budget < 1 {
		return string(runes[:maxLen])
	}
	cut := runes[:budget]
	floor := budget / 2
	for i := len(cut) - 1; i >= floor; i-- {
		switch cut[i] {
		case '.', '!', '?':
			if i+1 < len(runes) && runes[i+1] == ' ' {
				return string(cut[:i+1])
			}
		case 0x3002, 0xFF01, 0xFF1F: // CJK full stop, exclamation and question marks
			return string(cut[:i+1])
		}
	}
	for i := len(cut) - 1; i >= floor; i-- {
		if cut[i] == ' ' {
			return strings.TrimRight(string(cut[:i]), " ,;:") + "..."
		}
	}
	return string(cut) + "..."
}

// Extract builds a concise summary from the most recent conversation turns
// without calling any model. It prefers the latest substantial assistant
// message, then the latest user request, then the fallback title.
func Extract(turns []Turn, fallback string) string {
	var lastAssistant, lastUser string
	for i := len(turns) - 1; i >= 0; i-- {
		plain := Plain(turns[i].Text)
		if plain == "" {
			continue
		}
		switch turns[i].Role {
		case "assistant":
			if utf8.RuneCountInString(plain) >= 24 {
				return Truncate(plain, AutoMaxLen)
			}
			if lastAssistant == "" {
				lastAssistant = plain
			}
		case "user":
			if lastUser == "" {
				lastUser = plain
			}
		}
	}
	switch {
	case lastUser != "" && lastAssistant != "":
		return Truncate("Asked: "+lastUser+" Reply: "+lastAssistant, AutoMaxLen)
	case lastUser != "":
		return Truncate("Asked: "+lastUser, AutoMaxLen)
	case lastAssistant != "":
		return Truncate(lastAssistant, AutoMaxLen)
	}
	return Truncate(Plain(fallback), AutoMaxLen)
}
