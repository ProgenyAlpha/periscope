package store

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// CleanFirstPrompt strips slash commands, @mentions, HTML, collapses whitespace,
// capitalizes first char, and truncates at ~50 chars on word boundary.
func CleanFirstPrompt(raw string) string {
	s := raw
	s = reSlashCmd.ReplaceAllString(s, "")
	s = reAgentMention.ReplaceAllString(s, "")
	s = reHTMLTags.ReplaceAllString(s, "")
	s = reWhitespace.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)

	if s == "" {
		return raw // fallback to original if cleanup ate everything
	}

	// Capitalize first character (UTF-8 safe)
	if r, size := utf8.DecodeRuneInString(s); size > 0 {
		s = string(unicode.ToUpper(r)) + s[size:]
	}

	// Truncate at word boundary ~50 chars
	if len(s) > 50 {
		cut := 50
		for cut > 30 && s[cut] != ' ' {
			cut--
		}
		if s[cut] == ' ' {
			s = s[:cut] + "..."
		} else {
			s = s[:50] + "..."
		}
	}

	return s
}
