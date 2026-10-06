package diagnose

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// WriteText renders a report for a terminal.
func WriteText(w io.Writer, r *Report) error {
	var b strings.Builder
	if len(r.Diagnoses) == 0 {
		b.WriteString("No problems found.\n")
		_, err := io.WriteString(w, b.String())
		return err
	}
	for i, d := range r.Diagnoses {
		if i == 1 {
			fmt.Fprintf(&b, "\nAlso found (%d):\n", len(r.Diagnoses)-1)
		}
		if i > 0 {
			fmt.Fprintf(&b, "  - %s %s\n      %s\n", d.Code, d.Summary, d.Suggestion)
			continue
		}
		where := d.Subject
		if d.Stage != "" {
			where += ", stage " + d.Stage
		}
		fmt.Fprintf(&b, "%s: %s (%s)\n", d.Code, d.Title, where)
		fmt.Fprintf(&b, "  %s\n\n", d.Summary)
		fmt.Fprintf(&b, "  What to do: %s\n", d.Suggestion)
		if len(d.Evidence) > 0 {
			b.WriteString("\n  Evidence:\n")
			for _, e := range d.Evidence {
				fmt.Fprintf(&b, "    | %s\n", e)
			}
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteJSON renders a report as indented JSON.
func WriteJSON(w io.Writer, r *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

// CommentContext is what a pull-request comment says about the preview.
type CommentContext struct {
	// Environment names the preview (namespace or environment id).
	Environment string
	Generation  int64
	// Commit, when known, is shown shortened.
	Commit string
}

// WriteMarkdown renders a report as a pull-request comment. Everything that
// comes from the preview (log lines, object names, SQL identifiers) is
// placed in code spans or fenced blocks sized to what they contain, so app
// output cannot inject markdown or HTML into the comment.
func WriteMarkdown(w io.Writer, r *Report, c CommentContext) error {
	var b strings.Builder
	root := r.RootCause()
	footer := fmt.Sprintf("generation %d", c.Generation)
	if c.Environment != "" {
		footer = code(c.Environment) + " · " + footer
	}
	if c.Commit != "" {
		footer += " · commit " + code(shortSHA(c.Commit))
	}
	if root == nil {
		fmt.Fprintf(&b, "### Preview: no problems found\n\n<sub>%s</sub>\n", footer)
		_, err := io.WriteString(w, b.String())
		return err
	}
	fmt.Fprintf(&b, "### Preview failed: %s\n\n", root.Title)
	fmt.Fprintf(&b, "**%s**", code(string(root.Code)))
	if root.Subject != "" {
		fmt.Fprintf(&b, " in %s", code(root.Subject))
	}
	if root.Stage != "" {
		fmt.Fprintf(&b, " (stage: %s)", code(root.Stage))
	}
	b.WriteString("\n\n")
	b.WriteString(inline(root.Summary) + "\n\n")
	b.WriteString("**What to do:** " + inline(root.Suggestion) + "\n")
	if len(root.Evidence) > 0 {
		b.WriteString("\n<details><summary>Evidence</summary>\n\n")
		b.WriteString(fence(strings.Join(root.Evidence, "\n")))
		b.WriteString("\n</details>\n")
	}
	if rest := r.Diagnoses[1:]; len(rest) > 0 {
		fmt.Fprintf(&b, "\n<details><summary>Also found (%d)</summary>\n\n", len(rest))
		for _, d := range rest {
			fmt.Fprintf(&b, "- **%s** %s  \n  %s\n", code(string(d.Code)), inline(d.Summary), inline(d.Suggestion))
		}
		b.WriteString("\n</details>\n")
	}
	fmt.Fprintf(&b, "\n<sub>%s · run %s for details</sub>\n", footer, code("heimdall diagnose"))
	_, err := io.WriteString(w, b.String())
	return err
}

// code renders s as an inline code span that s cannot close early.
func code(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	ticks := strings.Repeat("`", longestRun(s, '`')+1)
	pad := ""
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		pad = " "
	}
	return ticks + pad + s + pad + ticks
}

// inline renders prose that may quote identifiers in backticks (rules write
// them that way) and may contain text from the preview: markdown and HTML
// metacharacters outside code spans are escaped.
func inline(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	parts := strings.Split(s, "`")
	if len(parts)%2 == 0 { // unbalanced: treat it all as text
		parts = []string{s}
	}
	var b strings.Builder
	for i, p := range parts {
		if i%2 == 1 {
			b.WriteString(code(p))
			continue
		}
		b.WriteString(escape(p))
	}
	return b.String()
}

var escaper = strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`, "<", "&lt;", ">", "&gt;",
	"#", `\#`, "|", `\|`, "~", `\~`, "`", "\\`", "!", `\!`)

func escape(s string) string { return escaper.Replace(s) }

// fence wraps text in a code fence longer than any backtick run inside it.
func fence(text string) string {
	ticks := strings.Repeat("`", max(3, longestRun(text, '`')+1))
	return ticks + "text\n" + text + "\n" + ticks + "\n"
}

func longestRun(s string, c byte) int {
	best, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			run++
			best = max(best, run)
		} else {
			run = 0
		}
	}
	return best
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
