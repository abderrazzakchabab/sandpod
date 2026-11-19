package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRenderPromptSubstitutesArgsAndRunsShellBlocks(t *testing.T) {
	var ran []string
	shell := func(c string) (string, error) { ran = append(ran, c); return "  out\n", nil }
	got, err := renderPrompt("Fix #{{ISSUE_NUMBER}} on {{ BRANCH }}\n!`git log -n {{N}}`", map[string]string{
		"ISSUE_NUMBER": "7", "BRANCH": "feat", "N": "3",
	}, shell)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Fix #7 on feat\nout" {
		t.Fatalf("got %q", got)
	}
	if !reflect.DeepEqual(ran, []string{"git log -n '3'"}) {
		t.Fatalf("ran %v", ran)
	}
}

func TestRenderPromptDoesNotRunShellBlocksFromArgs(t *testing.T) {
	shell := func(c string) (string, error) { t.Fatalf("ran %q", c); return "", nil }
	got, err := renderPrompt("{{X}}", map[string]string{"X": "!`rm -rf /`"}, shell)
	if err != nil || got != "!`rm -rf /`" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestRenderPromptQuotesArgsInsideShellBlocks(t *testing.T) {
	var ran string
	shell := func(c string) (string, error) { ran = c; return "", nil }
	_, err := renderPrompt("!`gh issue view {{T}}`", map[string]string{"T": "x'; rm -rf / #"}, shell)
	if err != nil {
		t.Fatal(err)
	}
	if want := `gh issue view 'x'\''; rm -rf / #'`; ran != want {
		t.Fatalf("ran %q, want %q", ran, want)
	}
}

func TestRenderPromptReportsMissingArgs(t *testing.T) {
	_, err := renderPrompt("{{A}} {{B}} {{A}}", map[string]string{}, nil)
	if err == nil || !strings.Contains(err.Error(), "A, B") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolvePrompt(t *testing.T) {
	repo := t.TempDir()
	for _, p := range []string{
		".sandcastle/implement-prompt.md",
		".factory/review-prompt.md",
		".sandcastle/agent-workflows/explore/prompt.md",
	} {
		full := filepath.Join(repo, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte("x"), 0o644)
	}
	for name, want := range map[string]string{
		"implement":        ".sandcastle/implement-prompt.md",
		"factory/review":   ".factory/review-prompt.md",
		"workflow/explore": ".sandcastle/agent-workflows/explore/prompt.md",
		"explore":          ".sandcastle/agent-workflows/explore/prompt.md",
	} {
		got, err := resolvePrompt(repo, name)
		if err != nil || got != filepath.Join(repo, want) {
			t.Errorf("%s: got %q, %v", name, got, err)
		}
	}
	if _, err := resolvePrompt(repo, "nope"); err == nil {
		t.Error("expected error for unknown prompt")
	}
	want := []string{"factory/review", "implement", "workflow/explore"}
	if got := listPrompts(repo); !reflect.DeepEqual(got, want) {
		t.Errorf("listPrompts = %v", got)
	}
}

func TestAttachedContainerURI(t *testing.T) {
	uri := attachedContainerURI("podagent-x-claude", "/home/agent/workspace")
	hexPart := strings.TrimSuffix(strings.TrimPrefix(uri, "vscode-remote://attached-container+"), "/home/agent/workspace")
	raw, err := hex.DecodeString(hexPart)
	if err != nil || string(raw) != `{"containerName":"/podagent-x-claude"}` {
		t.Fatalf("uri %q decoded %q (%v)", uri, raw, err)
	}
}

func TestAgentCommands(t *testing.T) {
	cases := map[string]struct {
		argv  []string
		stdin string
	}{
		"claude":   {[]string{"claude", "--print", "--dangerously-skip-permissions", "--model", "m"}, "P"},
		"codex":    {[]string{"codex", "exec", "--dangerously-bypass-approvals-and-sandbox", "--model", "m", "-"}, "P"},
		"opencode": {[]string{"opencode", "run", "--dangerously-skip-permissions", "--model", "m", "P"}, ""},
		"pi":       {[]string{"pi", "-p", "--model", "m"}, "P"},
	}
	for name, want := range cases {
		a, err := lookupAgent(name)
		if err != nil {
			t.Fatal(err)
		}
		argv, stdin := a.Print("m", "P")
		if !reflect.DeepEqual(argv, want.argv) || stdin != want.stdin {
			t.Errorf("%s: got %v %q", name, argv, stdin)
		}
		if cf := containerfile(a); !strings.Contains(cf, a.Install) || !strings.Contains(cf, "sleep") {
			t.Errorf("%s: containerfile missing install or entrypoint", name)
		}
	}
	if _, err := lookupAgent("cursor"); err == nil {
		t.Error("expected unknown agent error")
	}
}

func TestReadEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# c\nANTHROPIC_API_KEY=\nGH_TOKEN=\"abc\"\nexport X=1\n"), 0o644)
	got, err := readEnvFile(p)
	if err != nil || !reflect.DeepEqual(got, map[string]string{"GH_TOKEN": "abc", "X": "1"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestGitGuardMountsAreReadOnly(t *testing.T) {
	gitDir := filepath.Join(t.TempDir(), ".git")
	os.MkdirAll(gitDir, 0o755)
	mounts, err := gitGuardMounts(gitDir, "/home/agent/workspace/.git")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		gitDir + "/hooks:/home/agent/workspace/.git/hooks:ro,z",
		gitDir + "/config:/home/agent/workspace/.git/config:ro,z",
	}
	for i, m := range mounts {
		if m.flag() != want[i] {
			t.Errorf("mount %d = %q, want %q", i, m.flag(), want[i])
		}
	}
	if st, err := os.Stat(filepath.Join(gitDir, "hooks")); err != nil || !st.IsDir() {
		t.Error("hooks dir not created")
	}
}

func TestMountFlag(t *testing.T) {
	if got := (Mount{Host: "/a", Container: "/b"}).flag(); got != "/a:/b:z" {
		t.Error(got)
	}
	if got := (Mount{Host: "/a", Container: "/b", NoRelabel: true}).flag(); got != "/a:/b" {
		t.Error(got)
	}
}
