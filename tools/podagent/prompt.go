package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	placeholderRe = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
	// Same syntax as Sandcastle's PromptPreprocessor: !`command`
	shellBlockRe = regexp.MustCompile("!`([^`]+)`")
)

// resolvePrompt maps a prompt name to a file in the repo. Accepted forms:
//
//	./path/to/file.md          any existing file
//	implement                  .sandcastle/implement-prompt.md
//	factory/implement          .factory/implement-prompt.md
//	workflow/review            .sandcastle/agent-workflows/review/prompt.md
func resolvePrompt(repo, name string) (string, error) {
	candidates := []string{name, filepath.Join(repo, name)}
	switch {
	case strings.HasPrefix(name, "factory/"):
		n := strings.TrimPrefix(name, "factory/")
		candidates = append(candidates,
			filepath.Join(repo, ".factory", n+"-prompt.md"),
			filepath.Join(repo, ".factory", n+".md"))
	case strings.HasPrefix(name, "workflow/"):
		n := strings.TrimPrefix(name, "workflow/")
		candidates = append(candidates, filepath.Join(repo, ".sandcastle", "agent-workflows", n, "prompt.md"))
	default:
		candidates = append(candidates,
			filepath.Join(repo, ".sandcastle", name+"-prompt.md"),
			filepath.Join(repo, ".sandcastle", name+".md"),
			filepath.Join(repo, ".sandcastle", "agent-workflows", name, "prompt.md"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("prompt %q not found; try `podagent prompts`", name)
}

// listPrompts returns every prompt name resolvePrompt understands.
func listPrompts(repo string) []string {
	var out []string
	add := func(glob string, name func(string) string) {
		matches, _ := filepath.Glob(filepath.Join(repo, glob))
		for _, m := range matches {
			out = append(out, name(m))
		}
	}
	add(".sandcastle/*-prompt.md", func(p string) string {
		return strings.TrimSuffix(filepath.Base(p), "-prompt.md")
	})
	add(".factory/*-prompt.md", func(p string) string {
		return "factory/" + strings.TrimSuffix(filepath.Base(p), "-prompt.md")
	})
	add(".sandcastle/agent-workflows/*/prompt.md", func(p string) string {
		return "workflow/" + filepath.Base(filepath.Dir(p))
	})
	sort.Strings(out)
	return out
}

// Shell runs a command for a !`...` block and returns its stdout.
type Shell func(command string) (string, error)

// renderPrompt substitutes {{KEY}} arguments, then runs each !`command`
// block that appeared in the raw template (inside the container) and splices
// in its trimmed stdout. Blocks that only arrive through argument values are
// left as text, matching Sandcastle.
func renderPrompt(template string, args map[string]string, shell Shell) (string, error) {
	var missing []string
	seen := map[string]bool{}
	// quote wraps values in single quotes inside shell blocks, so an argument
	// such as an issue title can never inject shell syntax.
	substWith := func(s string, quote bool) string {
		return placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
			key := placeholderRe.FindStringSubmatch(m)[1]
			if v, ok := args[key]; ok {
				if quote {
					return shellQuote(v)
				}
				return v
			}
			if !seen[key] {
				seen[key] = true
				missing = append(missing, key)
			}
			return m
		})
	}
	subst := func(s string) string { return substWith(s, false) }

	locs := shellBlockRe.FindAllStringSubmatchIndex(template, -1)
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		b.WriteString(subst(template[last:loc[0]]))
		command := substWith(template[loc[2]:loc[3]], true)
		if shell == nil {
			b.WriteString(template[loc[0]:loc[1]])
		} else {
			out, err := shell(command)
			if err != nil {
				return "", fmt.Errorf("prompt shell block `%s`: %w", command, err)
			}
			b.WriteString(strings.TrimSpace(out))
		}
		last = loc[1]
	}
	b.WriteString(subst(template[last:]))

	if len(missing) > 0 {
		return "", fmt.Errorf("prompt needs arguments: %s (pass --arg KEY=VALUE)", strings.Join(missing, ", "))
	}
	return b.String(), nil
}

// shellQuote returns s as one single-quoted POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// parseArgs turns KEY=VALUE pairs into a map.
func parseArgs(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("bad --arg %q, want KEY=VALUE", p)
		}
		out[k] = v
	}
	return out, nil
}
