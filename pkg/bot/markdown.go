package bot

import (
	"fmt"
	"regexp"
	"strings"
)

// markdownToTelegramHTML converts standard LLM markdown to Telegram-compatible HTML.
// Telegram supports: <b>, <i>, <code>, <pre>, <a href="">, <s>, <u>
func markdownToTelegramHTML(md string) string {
	type codeBlock struct {
		lang, code string
	}
	var blocks []codeBlock
	var inlines []string

	// Phase 1: Extract fenced code blocks (greedy per block, handles nested backticks)
	fenced := regexp.MustCompile("(?sm)^```(\\w*)\\n(.*?)^```\\s*$")
	md = fenced.ReplaceAllStringFunc(md, func(match string) string {
		parts := fenced.FindStringSubmatch(match)
		idx := len(blocks)
		blocks = append(blocks, codeBlock{lang: parts[1], code: parts[2]})
		return fmt.Sprintf("\x00CB%d\x00", idx)
	})

	// Fallback: also catch inline-style ``` fences (no newlines around them)
	fencedInline := regexp.MustCompile("(?s)```(\\w*)\\n?(.*?)```")
	md = fencedInline.ReplaceAllStringFunc(md, func(match string) string {
		parts := fencedInline.FindStringSubmatch(match)
		idx := len(blocks)
		blocks = append(blocks, codeBlock{lang: parts[1], code: parts[2]})
		return fmt.Sprintf("\x00CB%d\x00", idx)
	})

	// Phase 2: Extract inline code spans (single backtick, no nesting)
	inlineCode := regexp.MustCompile("`([^`\n]+)`")
	md = inlineCode.ReplaceAllStringFunc(md, func(match string) string {
		parts := inlineCode.FindStringSubmatch(match)
		idx := len(inlines)
		inlines = append(inlines, parts[1])
		return fmt.Sprintf("\x00IC%d\x00", idx)
	})

	// Phase 3: Escape HTML entities in remaining text
	md = escapeHTML(md)

	// Phase 4: Markdown → HTML

	// Headers → bold
	md = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`).ReplaceAllString(md, "\n<b>$1</b>")

	// Bold **text** or __text__
	md = regexp.MustCompile(`\*\*(.+?)\*\*`).ReplaceAllString(md, "<b>$1</b>")
	md = regexp.MustCompile(`__(.+?)__`).ReplaceAllString(md, "<b>$1</b>")

	// Strikethrough ~~text~~
	md = regexp.MustCompile(`~~(.+?)~~`).ReplaceAllString(md, "<s>$1</s>")

	// Links [text](url)
	md = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`).ReplaceAllString(md, `<a href="$2">$1</a>`)

	// Bullet points at line start
	md = regexp.MustCompile(`(?m)^[\-\*]\s+`).ReplaceAllString(md, "• ")

	// Numbered list: keep as-is

	// Horizontal rules
	md = regexp.MustCompile(`(?m)^[\-\*]{3,}\s*$`).ReplaceAllString(md, "")

	// Phase 5: Restore inline code (HTML-escaped)
	for i, code := range inlines {
		esc := escapeHTML(code)
		md = strings.Replace(md, fmt.Sprintf("\x00IC%d\x00", i), "<code>"+esc+"</code>", 1)
	}

	// Phase 6: Restore fenced code blocks (HTML-escaped)
	for i, cb := range blocks {
		esc := strings.TrimRight(escapeHTML(cb.code), "\n ")
		if cb.lang != "" {
			md = strings.Replace(md, fmt.Sprintf("\x00CB%d\x00", i),
				fmt.Sprintf("<pre><code class=\"language-%s\">%s</code></pre>", cb.lang, esc), 1)
		} else {
			md = strings.Replace(md, fmt.Sprintf("\x00CB%d\x00", i),
				"<pre>"+esc+"</pre>", 1)
		}
	}

	// Clean up excessive blank lines
	md = regexp.MustCompile(`\n{3,}`).ReplaceAllString(md, "\n\n")

	return strings.TrimSpace(md)
}

// splitMarkdownChunks splits markdown text into chunks that respect code block
// boundaries. Never cuts inside a fenced code block.
func splitMarkdownChunks(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}

	lines := strings.Split(text, "\n")
	var chunks []string
	var buf strings.Builder
	inCode := false
	codeFence := ""

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Track code block boundaries
		if strings.HasPrefix(trimmed, "```") {
			if inCode {
				inCode = false
				codeFence = ""
			} else {
				inCode = true
				codeFence = trimmed
			}
		}

		lineLen := len(line) + 1
		wouldExceed := buf.Len()+lineLen > maxLen && buf.Len() > 0

		if wouldExceed && !inCode {
			// Safe to split here (outside code block)
			chunks = append(chunks, strings.TrimRight(buf.String(), "\n"))
			buf.Reset()
		} else if wouldExceed && inCode {
			// Inside a code block — close it, split, reopen
			buf.WriteString("\n```\n")
			chunks = append(chunks, strings.TrimRight(buf.String(), "\n"))
			buf.Reset()
			buf.WriteString(codeFence + "\n")
		}

		buf.WriteString(line)
		buf.WriteString("\n")
	}

	if buf.Len() > 0 {
		chunks = append(chunks, strings.TrimRight(buf.String(), "\n"))
	}

	return chunks
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
