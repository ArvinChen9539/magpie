package agent

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
)

func TestOpenCodeGatewayAliasStatusAndDisplay(t *testing.T) {
	const ref = "relay/glm-4.6"
	for _, agentID := range []string{"opencode", "openchamber"} {
		for _, alias := range []string{"relay-gateway", "relay", "relay.gateway"} {
			t.Run(agentID+"/"+alias, func(t *testing.T) {
				home := syncHome(t)
				openCodeAt(t, "2.3.2")
				cfg := filepath.Join(home, ".config")
				oc := opencode(home, cfg)
				a := oc
				if agentID == "openchamber" {
					a = openChamber(home, cfg)
				}
				saved := alias + "/" + ref
				writeProvider := func(id, base, model string) {
					t.Helper()
					path := "providers." + strings.ReplaceAll(id, ".", `\.`)
					if err := edit.SetJSON(oc.Path, edit.KV{Path: path, Value: map[string]any{
						"package": "aisdk:@ai-sdk/openai-compatible", "settings": map[string]string{"baseURL": base},
						"models": map[string]any{model: map[string]any{}},
					}}); err != nil {
						t.Fatal(err)
					}
				}
				if err := a.Apply("model", "magpie/"+ref); err != nil {
					t.Fatal(err)
				}
				if !a.Wired() {
					t.Fatal("initial Magpie provider should be connected")
				}
				writeProvider(alias, gatewayV1()+"/", ref)
				if err := a.Pick("model", "magpie/"+ref); err != nil {
					t.Fatal(err)
				}
				path, key := oc.Path, "model"
				if agentID == "openchamber" {
					path, key = filepath.Join(a.Dir, "settings.json"), "defaultModel"
				}
				check := func(wantWired bool) {
					t.Helper()
					if got, _ := edit.GetJSON(path, key); got != saved {
						t.Fatalf("configured model = %q, want %q", got, saved)
					}
					values := a.Values()
					if values["model"] != saved {
						t.Fatalf("saved model = %q, want %q", values["model"], saved)
					}
					if got := a.Wired(); got != wantWired {
						t.Errorf("Wired() = %v, want %v", got, wantWired)
					}
					if got := a.Models(); slices.Contains(got, ref) != wantWired {
						t.Errorf("Models() = %v, want presence of %s to be %v", got, ref, wantWired)
					}
					picks := a.Picks(*a.Field("model"), values)
					if got := slices.ContainsFunc(picks, func(option Option) bool {
						return option.Value == saved && option.Ref == ref && option.Label == "GLM-4.6"
					}); got != wantWired {
						t.Errorf("picker matches saved model to GLM-4.6 = %v, want %v", got, wantWired)
					}
				}
				check(true)
				writeProvider("another-gateway", gatewayV1(), ref)
				check(true)
				for _, invalid := range []struct{ name, base, model string }{
					{"different-endpoint", "https://example.test/v1", ref},
					{"unlisted-model", gatewayV1(), "glm-4.6"},
				} {
					t.Run(invalid.name, func(t *testing.T) {
						writeProvider(alias, invalid.base, invalid.model)
						check(false)
					})
				}
			})
		}
	}
}
