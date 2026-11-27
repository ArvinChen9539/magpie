package library

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// A plugin with only Codex's manifest (#7255) is imported like a Claude Code
// one: its skills and servers by the manifest's paths, and the hooks it names
// are Codex's own, given to Codex alone, as written.
func TestPluginCodexManifest(t *testing.T) {
	h := sandbox(t)
	pluginAgents(t, h)
	src := filepath.Join(t.TempDir(), "cx")
	write(t, filepath.Join(src, ".codex-plugin/plugin.json"), `{"name":"cx-kit","version":"0.3.0","description":"For Codex","skills":"./skills/","mcpServers":"./.mcp.json","hooks":"./codex-hooks.json"}`)
	write(t, filepath.Join(src, "skills/tdd/SKILL.md"), "---\nname: tdd\ndescription: Test first\n---\nWrite the test first.\n")
	write(t, filepath.Join(src, ".mcp.json"), `{"mcpServers":{"cx-db":{"command":"db"}}}`)
	write(t, filepath.Join(src, "codex-hooks.json"), `{"hooks":{
	  "PermissionRequest":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo ask"}]}],
	  "Stop":[{"hooks":[{"type":"command","command":"echo done"}]}],
	  "Nonsense":[{"hooks":[{"type":"command","command":"echo no"}]}]}}`)

	p, err := ProbePlugins(src)
	if err != nil || len(p.Candidates) != 1 {
		t.Fatalf("probe: %v %+v", err, p)
	}
	c := p.Candidates[0]
	if c.Name != "cx-kit" || c.Skills != 1 || c.MCP != 1 || c.Hooks != 2 || len(c.Left) != 1 {
		t.Fatalf("candidate: %+v", c)
	}
	ok(t)(InstallPlugins(src, nil, []string{"claude", "codex"}))
	codex := read(t, filepath.Join(h, ".codex/hooks.json"))
	for _, want := range []string{"PermissionRequest", "echo ask", "Stop", "echo done"} {
		if !strings.Contains(codex, want) {
			t.Errorf("codex hooks lack %q:\n%s", want, codex)
		}
	}
	if s := read(t, filepath.Join(h, ".claude/settings.json")); strings.Contains(s, "echo ask") || strings.Contains(s, "echo done") {
		t.Errorf("claude was given Codex's hooks:\n%s", s)
	}
	if !strings.Contains(read(t, filepath.Join(h, ".codex/config.toml")), "cx-db") {
		t.Errorf("codex lacks the server")
	}
	ok(t)(RemovePlugin("cx-kit"))
	if s := read(t, filepath.Join(h, ".codex/hooks.json")); strings.Contains(s, "echo ask") {
		t.Errorf("hooks stay after remove:\n%s", s)
	}
}

func TestPluginCodexHookGetsBundleEnvironment(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if cmdOS == "windows" {
		shell = gitBash()
		if shell == "" {
			t.Skip("Git Bash is needed to exercise the POSIX hook on Windows")
		}
	} else if err != nil {
		t.Fatal(err)
	}
	wasOS := cmdOS
	cmdOS = "linux"
	t.Cleanup(func() { cmdOS = wasOS })
	home := sandbox(t)
	pluginAgents(t, home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config with ' and $ signs"))
	source := filepath.Join(t.TempDir(), "cx")
	write(t, filepath.Join(source, ".codex-plugin/plugin.json"), `{"name":"cx-kit","hooks":"./codex-hooks.json"}`)
	write(t, filepath.Join(source, "codex-hooks.json"), `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"sh \"${PLUGIN_ROOT}/probe.sh\" \"$PLUGIN_ROOT\" \"${PLUGIN_DATA}\" \"$PLUGIN_DATA\"","timeout":30}]}]}}`)
	write(t, filepath.Join(source, "probe.sh"), "printf '%s\\n' \"$@\" \"$PLUGIN_ROOT\" \"$PLUGIN_DATA\"\n")
	config := filepath.Join(home, ".codex/config.toml")
	hooksPath := filepath.Join(home, ".codex/hooks.json")
	const userHooks = `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo user's hook"}]}]}}`
	write(t, hooksPath, userHooks)
	write(t, config, "[hooks.state."+codexKey(hooksPath+":pre_tool_use:0:0")+"]\ntrusted_hash = \"sha256:user-hook\"\n")
	beforeUserHooks := hooksOf(t, hooksPath, "PreToolUse")
	before := read(t, config)
	ok(t)(InstallPlugins(source, nil, []string{"codex"}))
	hooks := read(t, hooksPath)
	if got := hooksOf(t, hooksPath, "PreToolUse"); len(got) != 1 || len(beforeUserHooks) != 1 || got[0] != beforeUserHooks[0] {
		t.Fatalf("importing a hook changed the user's own hook: %v", got)
	}
	command := gjson.Get(hooks, "hooks.Stop.0.hooks.0.command").String()
	if gjson.Get(hooks, "hooks.Stop.0.hooks.0.timeout").Int() != 30 || command == "" {
		t.Fatalf("missing Stop command or timeout in %s", hooks)
	}
	if after := read(t, config); after != before {
		t.Fatalf("importing a hook changed Codex's trust configuration:\n%s", after)
	}
	cmd := exec.Command(shell, "-c", command)
	cmd.Dir = home
	for _, env := range os.Environ() {
		upper := strings.ToUpper(env)
		if !strings.HasPrefix(upper, "PLUGIN_ROOT=") && !strings.HasPrefix(upper, "PLUGIN_DATA=") && !strings.HasPrefix(upper, "PATH=") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "PATH="+filepath.Dir(shell))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the exported hook did not run: %v\n%s", err, output)
	}
	root, data := filepath.ToSlash(pluginDir("cx-kit")), filepath.ToSlash(pluginData("cx-kit"))
	want := strings.Join([]string{root, data, data, root, data, ""}, "\n")
	if got := strings.ReplaceAll(string(output), "\r\n", "\n"); got != want {
		t.Fatalf("hook args and child environment = %q, want %q", got, want)
	}
}
