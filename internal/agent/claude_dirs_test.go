package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

// BieJian21872 on X: a second Claude Code (CLAUDE_CONFIG_DIR=~/.claude-work)
// was never found; only ~/.claude was. Each extra folder holding Claude
// Code's files is a row of its own, connected in its own settings.json.
func TestClaudeCodeInMoreThanOneFolder(t *testing.T) {
	home, _ := claudeDottedHome(t, provider.Provider{ID: "relay", Name: "Relay", Anthropic: "https://example.test", Key: "k",
		Models: []string{"claude-sonnet-4.5", "glm-4.6"}}, `{}`)
	work := filepath.Join(home, ".claude-work")
	writeFile(t, filepath.Join(work, "settings.json"), `{"theme": "dark"}`)
	writeFile(t, filepath.Join(home, ".claude2", "projects", "p", "s.jsonl"), `{}`)
	// not Claude Code's: another tool's folder, a file, a backup, the launch file's name
	writeFile(t, filepath.Join(home, ".claude-code-router", "config.json"), `{}`)
	writeFile(t, filepath.Join(home, ".claude.json"), `{}`)
	writeFile(t, filepath.Join(home, ".claude.json.backup"), `{}`)
	writeFile(t, filepath.Join(home, ".claude-launch", "settings.json"), `{}`)
	t.Setenv("PATH", t.TempDir())

	got := map[string]*Agent{}
	for _, a := range claudeDirAgents(home) {
		got[a.ID] = a
	}
	if len(got) != 2 || got["claude#work"] == nil || got["claude#2"] == nil {
		t.Fatalf("rows %v, want claude#work and claude#2 only", got)
	}
	// listed on the Agents page with the others
	listed := false
	for _, b := range onMachine(home, filepath.Join(home, ".config")) {
		listed = listed || b.ID == "claude#work"
	}
	if !listed {
		t.Fatal("claude#work isn't among the agents magpie lists")
	}
	a := got["claude#work"]
	if a.Dir != work || !a.Detected() || a.Name != "Claude Code · work" {
		t.Fatalf("row %q at %s detected %v", a.Name, a.Dir, a.Detected())
	}
	if err := a.Field("model").Set("relay/glm-4.6"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(work, "settings.json")
	if v, _ := edit.GetJSON(path, "env.ANTHROPIC_BASE_URL"); v != gateway.URL() {
		b, _ := os.ReadFile(path)
		t.Fatalf("%s doesn't point at magpie:\n%s", path, b)
	}
	if v, _ := edit.GetJSON(path, "theme"); v != "dark" {
		t.Fatalf("the user's theme is %q", v)
	}
	// the default row is untouched
	if v, _ := edit.GetJSON(filepath.Join(home, ".claude", "settings.json"), "env.ANTHROPIC_BASE_URL"); v != "" {
		t.Fatalf("~/.claude was written: %q", v)
	}
	if !claude(home).Detected() {
		t.Fatal("the default row is gone")
	}

	// with CLAUDE_CONFIG_DIR at one of them, the others (~/.claude too) are rows
	t.Setenv("CLAUDE_CONFIG_DIR", work)
	ids := map[string]bool{}
	for _, a := range claudeDirAgents(home) {
		ids[a.ID] = true
	}
	if ids["claude#work"] || !ids["claude#default"] || !ids["claude#2"] {
		t.Fatalf("rows with CLAUDE_CONFIG_DIR at .claude-work: %v", ids)
	}
}

func TestClaudeProfileSharedAttributionDoesNotReportBypass(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_test","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"Hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)
	home, _ := claudeDottedHome(t, provider.Provider{ID: "relay", Anthropic: upstream.URL, Key: "k",
		Models: []string{"claude-sonnet-4-5"}}, `{}`)
	t.Setenv("MAGPIE_ADDR", gateway.DefaultAddr)
	managed := claudeManaged
	claudeManaged = func() string { return filepath.Join(home, "managed-settings.json") }
	t.Cleanup(func() { claudeManaged = managed })
	work := filepath.Join(home, ".claude-work")
	settingsPath := filepath.Join(work, "settings.json")
	writeFile(t, settingsPath, `{}`)
	profiles := claudeDirAgents(home)
	if len(profiles) != 1 || profiles[0].ID != "claude#work" {
		t.Fatalf("expected claude#work, got %v", profiles)
	}
	profile := profiles[0]
	if err := profile.Apply("model", "relay/claude-sonnet-4-5"); err != nil {
		t.Fatal(err)
	}
	if !profile.Wired() {
		t.Fatal("profile is not wired after Apply")
	}

	promptAt := time.Now().Add(-time.Minute)
	priorStart := started
	started = promptAt.Add(-2 * time.Minute)
	t.Cleanup(func() { started = priorStart })
	records := appliedLoad()
	record := records[profile.ID]
	record.At = promptAt.Add(-time.Minute)
	records[profile.ID] = record
	appliedSave(records)
	writeFile(t, filepath.Join(work, "history.jsonl"), fmt.Sprintf(`{"display":"hi","timestamp":%d,"project":"/Users/me/work","sessionId":"profile-session"}`+"\n", promptAt.UnixMilli()))

	base, _ := edit.GetJSON(settingsPath, "env.ANTHROPIC_BASE_URL")
	token, _ := edit.GetJSON(settingsPath, "env.ANTHROPIC_AUTH_TOKEN")
	req := httptest.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(fmt.Sprintf(
		`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, writtenModel(settingsPath))))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "claude-cli/2.1.316 (external, cli)")
	response := httptest.NewRecorder()
	server := gateway.New()
	server.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("profile request returned %d: %s", response.Code, response.Body)
	}
	if calls := server.Recent(); len(calls) != 1 || calls[0].Agent != "claude" {
		t.Fatalf("expected the profile request under claude, got %+v", calls)
	}
	if seen := usage.LastSeen(profile.ID); !seen.IsZero() {
		t.Fatalf("unexpected profile attribution at %v", seen)
	}
	if drift := profile.DriftShown(); drift != nil {
		t.Errorf("wired profile with shared attribution showed drift: %+v", drift)
	}

	if err := edit.SetJSON(settingsPath, edit.KV{Path: "env.ANTHROPIC_BASE_URL", Value: "https://elsewhere.example"}); err != nil {
		t.Fatal(err)
	}
	if drift := profile.DriftShown(); drift == nil || drift.Kind != "unwired" {
		t.Errorf("changed endpoint should show unwired drift, got %+v", drift)
	}
}
