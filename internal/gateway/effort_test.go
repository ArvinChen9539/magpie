package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

func TestModelEffortOverrideReachesUpstream(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	fakeUpstream := &fake{t: t, reply: chatHi}
	up := setup(t, provider.Chat, fakeUpstream)
	writeCatalog(t, `{"opencode-go":{"models":{"deepseek-v4.1-flash":{
		"id":"deepseek-v4.1-flash","reasoning":true,"reasoning_options":[{"type":"effort","values":["low","high","max"]}]
	}}}}`)
	for _, vendor := range []struct{ id, catalog string }{{"opencode-go", "opencode-go"}, {"relay", ""}, {"other-relay", ""}} {
		if err := provider.Save(provider.Provider{
			ID: vendor.id, Name: vendor.id, Catalog: vendor.catalog, Key: "k",
			Chat: up.URL + "/v1", Models: []string{model},
		}); err != nil {
			t.Fatal(err)
		}
	}
	openCode, relay, other := "opencode-go/"+model, "relay/"+model, "other-relay/"+model
	if err := provider.SaveGroup(provider.Group{ID: "flash", Name: "Flash", Members: []string{relay}}); err != nil {
		t.Fatal(err)
	}
	server := New()
	check := func(target, want string) {
		t.Helper()
		code, body := postTo(t, server, "/v1/responses", `{"model":"`+target+`","stream":true,"input":"hi","reasoning":{"effort":"none"}}`)
		if code != 200 {
			t.Fatalf("%s: status %d: %s", target, code, body)
		}
		var sent struct {
			Model  string `json:"model"`
			Effort string `json:"reasoning_effort"`
		}
		if err := json.Unmarshal(fakeUpstream.got, &sent); err != nil {
			t.Fatal(err)
		}
		if sent.Model != model || sent.Effort != want || fakeUpstream.path != "/v1/chat/completions" {
			t.Errorf("%s: sent %s to %s, want %s at %s on Chat Completions", target, fakeUpstream.got, fakeUpstream.path, model, want)
		}
	}
	for _, target := range []string{openCode, relay, "group/flash", other} {
		check(target, "low")
	}
	for _, target := range []string{openCode, relay} {
		if err := provider.SetModelSpec(target, provider.SpecChange{ReasoningOverride: new("none,low,high,max")}); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{openCode, relay, "group/flash"} {
		check(target, "none")
	}
	check(other, "low")
	models := httptest.NewRecorder()
	server.Handler().ServeHTTP(models, httptest.NewRequest("GET", "/v1/models", nil))
	var listed struct {
		Data []struct {
			ID     string   `json:"id"`
			Levels []string `json:"reasoning_levels"`
		} `json:"data"`
	}
	if err := json.Unmarshal(models.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{openCode: "none,low,high,max", relay: "none,low,high,max", "group/flash": "none,low,high,max", other: "low,high,max"}
	for _, model := range listed.Data {
		if levels, ok := want[model.ID]; ok {
			if got := strings.Join(model.Levels, ","); got != levels {
				t.Errorf("%s advertises %s, want %s", model.ID, got, levels)
			}
			delete(want, model.ID)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing models: %v", want)
	}
	if err := provider.SetModelSpec(relay, provider.SpecChange{ReasoningOverride: new("auto")}); err != nil {
		t.Fatal(err)
	}
	check(relay, "low")
	check("group/flash", "low")
	check(openCode, "none")
}

func TestModelEffortOverrideReachesAnthropic(t *testing.T) {
	const model = "claude-sonnet-5"
	const target = "relay/" + model
	fakeUpstream := &fake{t: t, reply: anthropicHi}
	up := setup(t, provider.Anthropic, fakeUpstream)
	writeCatalog(t, `{"anthropic":{"models":{"claude-sonnet-5":{
		"id":"claude-sonnet-5","reasoning":true,"reasoning_options":[{"type":"effort","values":["low","medium","high","xhigh","max"]}]
	}}}}`)
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Key: "k", Anthropic: up.URL, Models: []string{model}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetModelSpec(target, provider.SpecChange{ReasoningOverride: new("none,low,medium,high,xhigh,max")}); err != nil {
		t.Fatal(err)
	}
	server := New()
	check := func(want string) {
		t.Helper()
		code, body := postTo(t, server, "/v1/responses", `{"model":"`+target+`","stream":true,"input":"hi","reasoning":{"effort":"none"}}`)
		if code != 200 {
			t.Fatalf("status %d: %s", code, body)
		}
		var sent struct {
			Thinking struct {
				Type string `json:"type"`
			} `json:"thinking"`
		}
		if err := json.Unmarshal(fakeUpstream.got, &sent); err != nil {
			t.Fatal(err)
		}
		if sent.Thinking.Type != want {
			t.Errorf("thinking type %q, want %q: %s", sent.Thinking.Type, want, fakeUpstream.got)
		}
	}
	check("disabled")
	if err := provider.SetModelSpec(target, provider.SpecChange{Reasoning: new("off")}); err != nil {
		t.Fatal(err)
	}
	check("")
}

func TestFitEffort(t *testing.T) {
	glm := []string{"low", "high", "max"}
	for _, c := range []struct {
		want   string
		levels []string
		got    string
	}{
		{"medium", glm, "high"}, // a tie goes up
		{"low", glm, "low"},
		{"xhigh", glm, "max"},
		{"xhigh", []string{"high", "max"}, "max"}, // DeepSeek V4: Ultracode's xhigh
		{"max", []string{"low", "medium", "high"}, "high"},
		{"low", []string{"none", "minimal", "medium"}, "medium"},
		{"medium", nil, "medium"}, // levels not known: as asked
		{"medium", []string{"none"}, "medium"},
	} {
		if got := fitEffort(c.want, c.levels); got != c.got {
			t.Errorf("fitEffort(%q, %v) = %q, want %q", c.want, c.levels, got, c.got)
		}
	}
}

// Codex's effort reaches a custom OpenAI-compatible provider as
// reasoning_effort, at a level the model takes: glm-5.3-flash takes low,
// high and max, as models.dev has it from those serving it, so Codex's
// "medium" goes as "high"; a model no one gives levels for gets what was
// asked.
func TestResponsesEffortReachesChatVendor(t *testing.T) {
	f := &fake{t: t, reply: sse(
		`data: {"id":"c1","model":"glm-5.3-flash","choices":[{"delta":{"role":"assistant","content":"hi"}}]}`,
		`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`)}
	up := setup(t, provider.Chat, f)
	os.MkdirAll(filepath.Dir(catalog.CachePath()), 0o755)
	os.WriteFile(catalog.CachePath(), []byte(`{"zai":{"models":{"glm-5.3-flash":{"id":"glm-5.3-flash","reasoning_options":[{"type":"effort","values":["low","high","max"]}]}}}}`), 0o644)
	catalog.Reset()
	t.Cleanup(catalog.Reset)
	if err := provider.Save(provider.Provider{ID: "volc", Name: "Volc", Key: "k", Chat: up.URL + "/v1", Models: []string{"glm-5.3-flash", "other"}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ model, effort, sent string }{
		{"volc/glm-5.3-flash", "medium", "high"},
		{"volc/glm-5.3-flash", "max", "max"},
		{"volc/glm-5.3-flash", "low", "low"},
		{"volc/other", "medium", "medium"},
	} {
		code, body := post(t, "/v1/responses", `{"model":"`+c.model+`","stream":true,"input":"hi","reasoning":{"effort":"`+c.effort+`"}}`)
		if code != 200 {
			t.Fatalf("status %d: %s", code, body)
		}
		var got map[string]any
		json.Unmarshal(f.got, &got)
		if got["reasoning_effort"] != c.sent || !strings.HasSuffix(f.path, "/chat/completions") {
			t.Errorf("%s at %s: sent %s", c.model, c.effort, f.got)
		}
	}
}

// A model its maker lists with a thinking switch alone — Xiaomi's
// mimo-v2.6-flash, served here by a relay — is sent no more than high: an
// agent asking max or xhigh of it got Xiaomi's 400 (#214). What it may take
// below that goes as asked, as does max to a model no one lists.
func TestSwitchModelSentNoMoreThanHigh(t *testing.T) {
	f := &fake{t: t, reply: sse(
		`data: {"id":"c1","model":"mimo-v2.6-flash","choices":[{"delta":{"role":"assistant","content":"hi"}}]}`,
		`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`)}
	up := setup(t, provider.Chat, f)
	os.MkdirAll(filepath.Dir(catalog.CachePath()), 0o755)
	os.WriteFile(catalog.CachePath(), []byte(`{
	  "xiaomi":{"models":{"mimo-v2.6-flash":{"id":"mimo-v2.6-flash","reasoning":true,"reasoning_options":[{"type":"toggle"}]}}},
	  "llmgateway":{"models":{"mimo-v2.6-flash":{"id":"mimo-v2.6-flash","reasoning_options":[{"type":"effort","values":["none","low","medium","high","xhigh","max"]}]}}},
	  "zai":{"models":{"glm-5.3-flash":{"id":"glm-5.3-flash","reasoning_options":[{"type":"effort","values":["low","high","max"]}]}}}}`), 0o644)
	catalog.Reset()
	t.Cleanup(catalog.Reset)
	if err := provider.Save(provider.Provider{ID: "volc", Name: "Volc", Key: "k", Chat: up.URL + "/v1", Models: []string{"mimo-v2.6-flash", "glm-5.3-flash", "other"}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ model, effort, sent string }{
		{"volc/mimo-v2.6-flash", "max", "high"},
		{"volc/mimo-v2.6-flash", "xhigh", "high"},
		{"volc/mimo-v2.6-flash", "low", "low"},
		{"volc/glm-5.3-flash", "medium", "high"},
		{"volc/other", "max", "max"},
	} {
		code, body := post(t, "/v1/responses", `{"model":"`+c.model+`","stream":true,"input":"hi","reasoning":{"effort":"`+c.effort+`"}}`)
		if code != 200 {
			t.Fatalf("status %d: %s", code, body)
		}
		var got map[string]any
		json.Unmarshal(f.got, &got)
		if got["reasoning_effort"] != c.sent {
			t.Errorf("%s at %s: sent %s", c.model, c.effort, f.got)
		}
	}
}

func TestToggleOffOnlyReachesVerifiedProvider(t *testing.T) {
	noRests()
	t.Cleanup(noRests)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	body, err := os.ReadFile(filepath.Join("..", "catalog", "testdata", "models.dev-2026-10-09.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(catalog.CachePath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalog.CachePath(), body, 0o644); err != nil {
		t.Fatal(err)
	}
	catalog.Reset()
	t.Cleanup(catalog.Reset)

	reply := sse(
		`data: {"id":"c1","model":"deepseek-v4-pro","choices":[{"delta":{"role":"assistant","content":"ok"}}]}`,
		`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	)
	deepseek := &fake{t: t, reply: reply}
	deepseekUp := httptest.NewServer(deepseek)
	t.Cleanup(deepseekUp.Close)
	relay := &fake{t: t, reply: reply}
	relayUp := httptest.NewServer(relay)
	t.Cleanup(relayUp.Close)
	for _, p := range []provider.Provider{
		{ID: "deepseek", Name: "DeepSeek", Key: "kd", Chat: deepseekUp.URL + "/v1", Catalog: "deepseek", Models: []string{"deepseek-v4-pro"}},
		{ID: "relay", Name: "Relay", Key: "kr", Chat: relayUp.URL + "/v1", Models: []string{"deepseek-v4-pro"}},
	} {
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := catalog.SaveLive("relay", relayUp.URL+"/v1", []catalog.Model{{ID: "deepseek-v4-pro"}}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		model string
		up    *fake
		want  string
	}{
		{"deepseek/deepseek-v4-pro", deepseek, "none"},
		{"relay/deepseek-v4-pro", relay, "low"},
	} {
		request := `{"model":"` + tc.model + `","stream":true,"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`
		code, response := post(t, "/v1/chat/completions", request)
		if code != 200 {
			t.Fatalf("%s: status %d: %s", tc.model, code, response)
		}
		var sent map[string]any
		if err := json.Unmarshal(tc.up.got, &sent); err != nil {
			t.Fatalf("%s: invalid upstream request %s: %v", tc.model, tc.up.got, err)
		}
		if sent["reasoning_effort"] != tc.want {
			t.Errorf("%s sent reasoning_effort %v, want %q", tc.model, sent["reasoning_effort"], tc.want)
		}
	}
}

// Each request's route keeps the reasoning the agent asked for, and each
// try the reasoning its model was sent at, fitted to the model's levels;
// the usage keeps what the model was sent at, by session.
func TestTraceRecordsTheEffortSent(t *testing.T) {
	f := &fake{t: t, reply: sse(
		`data: {"id":"c1","model":"glm-5.3-flash","choices":[{"delta":{"role":"assistant","content":"hi"}}]}`,
		`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
		`data: [DONE]`)}
	up := setup(t, provider.Chat, f)
	os.MkdirAll(filepath.Dir(catalog.CachePath()), 0o755)
	os.WriteFile(catalog.CachePath(), []byte(`{"zai":{"models":{"glm-5.3-flash":{"id":"glm-5.3-flash","reasoning_options":[{"type":"effort","values":["low","high","max"]}]}}}}`), 0o644)
	catalog.Reset()
	t.Cleanup(catalog.Reset)
	if err := provider.Save(provider.Provider{ID: "volc", Name: "Volc", Key: "k", Chat: up.URL + "/v1", Models: []string{"glm-5.3-flash", "other"}}); err != nil {
		t.Fatal(err)
	}
	s := New()
	for _, c := range []struct{ path, body, asked, sent string }{
		{"/v1/responses", `{"model":"volc/glm-5.3-flash","stream":true,"input":"hi","reasoning":{"effort":"medium"}}`, "medium", "high"},
		{"/v1/chat/completions", `{"model":"volc/other","stream":true,"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh"}`, "xhigh", "xhigh"},
		{"/v1/messages", `{"model":"volc/glm-5.3-flash","stream":true,"max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":20000},"messages":[{"role":"user","content":"hi"}]}`, "high", "high"},
		{"/v1/messages", `{"model":"volc/other","stream":true,"max_tokens":32000,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`, "max", "max"},
		{"/v1/chat/completions", `{"model":"volc/other","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "", ""},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", c.path, strings.NewReader(c.body))
		req.Header.Set(SessionHeader, "sess-1")
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", c.body, rec.Code, rec.Body)
		}
		r := lastRoute(s)
		if r.Effort != c.asked || len(r.Tries) != 1 || r.Tries[0].Effort != c.sent || r.Tries[0].Picked {
			t.Errorf("%s: route at %q, tries %+v; want asked %q, sent %q", c.body, r.Effort, r.Tries, c.asked, c.sent)
		}
	}
	var got []string
	for _, rec := range usage.Load(time.Time{}) {
		got = append(got, rec.Effort)
	}
	if strings.Join(got, ",") != "high,xhigh,high,max," {
		t.Errorf("usage efforts %q", got)
	}
	vs := usage.Vias(time.Time{}).Of(usage.AgentOf("") + "|sess-1")
	if len(vs) != 4 || vs[0].Model != "glm-5.3-flash" || vs[0].Effort != "high" || vs[0].Calls != 2 {
		t.Errorf("vias %+v", vs)
	}
}

// A Gemini request's thinkingConfig is the reasoning it asks for, read
// as parseGemini reads it (#4529): its level, or its budget as the level
// it is nearest, by either spelling of the fields' names.
func TestGeminiRequestEffort(t *testing.T) {
	for _, c := range []struct {
		body string
		want string
	}{
		{`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}`, "high"},
		{`{"generationConfig":{"thinkingConfig":{"thinking_level":"LOW"}}}`, "low"},
		{`{"generationConfig":{"thinkingConfig":{"thinkingBudget":512}}}`, "low"},
		{`{"generationConfig":{"thinkingConfig":{"thinking_budget":32768}}}`, "xhigh"},
		{`{"generationConfig":{"thinkingConfig":{"thinkingBudget":-1}}}`, "medium"},
		{`{"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`, ""},
		{`{"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`, ""},
		{`{"generationConfig":{}}`, ""},
		{`{"contents":[{"parts":[{"text":"hi"}]}]}`, ""},
	} {
		if got := requestEffort(provider.Gemini, []byte(c.body)); got != c.want {
			t.Errorf("%s: requestEffort %q, want %q", c.body, got, c.want)
		}
		r, err := parseGemini([]byte(c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		if r.Effort != c.want {
			t.Errorf("%s: parseGemini effort %q, want %q", c.body, r.Effort, c.want)
		}
	}
}

// requestEffort reads the reasoning a request asks for in each API's words.
func TestRequestEffort(t *testing.T) {
	for _, c := range []struct {
		proto provider.Protocol
		body  string
		want  string
	}{
		{provider.Chat, `{"reasoning_effort":"low"}`, "low"},
		{provider.Chat, `{"model":"m"}`, ""},
		{provider.Responses, `{"reasoning":{"effort":"xhigh","summary":"auto"}}`, "xhigh"},
		{provider.Responses, `{"reasoning":{"summary":"auto"}}`, ""},
		{provider.Anthropic, `{"thinking":{"type":"adaptive"},"output_config":{"effort":"medium"}}`, "medium"},
		{provider.Anthropic, `{"thinking":{"type":"enabled","budget_tokens":4096}}`, "low"},
		{provider.Anthropic, `{"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`, ""},
		{provider.Anthropic, `{"output_config":{"effort":"high"}}`, ""},
	} {
		if got := requestEffort(c.proto, []byte(c.body)); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.proto, c.body, got, c.want)
		}
	}
}
