package usage

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/sessions"
	"github.com/yetone/magpie/internal/settings"
)

func TestAntigravityTieredHydrogenUsagePrice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	path := catalog.CachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(catalog.Reset)
	setCatalog := func(models string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(`{"google":{"models":{`+models+`}}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		catalog.Reset()
	}
	const canonical = `"gemini-4-hydrogen":{"id":"gemini-4-hydrogen","cost":{
		"input":1.5,"output":7.5,"cache_read":0.075,"cache_write":1.5,
		"tiers":[{"tier":{"type":"context","size":200000},"input":3,"output":12,"cache_read":0.2,"cache_write":4}]}}`
	const alias = "gemini-4-hydrogen-tiered-high"
	setCatalog(canonical)

	now := holdClock(t, time.Date(2026, 11, 23, 12, 0, 0, 0, time.Local))
	tokens := sessions.Tokens{Input: 250000, Output: 20000, CacheRead: 40000, CacheWrite: 10000}
	local := sessions.Call{Time: now.Add(-time.Minute), Agent: "antigravity", Session: "desktop", Model: alias, Tokens: tokens}
	old := LogCalls
	LogCalls = func(time.Time) []sessions.Call {
		return []sessions.Call{
			{Time: now.Add(-2 * time.Minute), Agent: "agy", Session: "cli", Model: alias, Tokens: tokens},
			local,
			{Time: now.Add(-3 * time.Minute), Agent: "antigravity", Session: "unverified", Model: "gemini-4-hydrogen-tiered-low", Tokens: tokens},
		}
	}
	t.Cleanup(func() { LogCalls = old })
	Append(Record{Time: now.Add(-time.Hour), Agent: "antigravity", Provider: "relay", Session: "gateway", Model: alias, Input: 250000, Output: 20000, Status: 200})

	check := func(wantPriced bool, wantCost float64) {
		t.Helper()
		rows, _, _ := Ledger(Month, Filter{})
		if len(rows) != 4 {
			t.Fatalf("got %d rows, want 4", len(rows))
		}
		for _, row := range rows {
			if row.Session == "desktop" {
				if row.Model != alias || row.Served != alias || row.Provider != UnknownProvider || row.Source != "log" {
					t.Errorf("local row lost its original identity: %+v", row)
				}
				if row.Priced != wantPriced || math.Abs(row.Cost-wantCost) > 1e-12 {
					t.Errorf("desktop priced=%v cost=%v, want priced=%v cost=%v", row.Priced, row.Cost, wantPriced, wantCost)
				}
			} else if row.Priced {
				t.Errorf("%s unexpectedly priced at %v", row.Session, row.Cost)
			}
		}
		pageRow := sessionRowPricer(settings.Load())(logRecord(local), "log")
		if pageRow.Model != alias || pageRow.Served != alias || pageRow.Priced != wantPriced || math.Abs(pageRow.Cost-wantCost) > 1e-12 {
			t.Errorf("request row model=%q served=%q priced=%v cost=%v, want %q, priced=%v cost=%v", pageRow.Model, pageRow.Served, pageRow.Priced, pageRow.Cost, alias, wantPriced, wantCost)
		}
	}
	base := catalogBase()
	check(true, 1.038)
	setCatalog(canonical + `,"unrelated":{"id":"unrelated","cost":{"input":1}}`)
	if got := catalogBase(); got != base {
		t.Error("an unrelated catalog change invalidated the request index")
	}
	setCatalog(`"gemini-4-hydrogen":{"id":"gemini-4-hydrogen","cost":{"input":2}}`)
	if got := catalogBase(); got == base {
		t.Error("an updated Hydrogen price did not invalidate the request index")
	}
	check(true, 0.5)
	setCatalog(`"gemini-4-hydrogen":{"id":"gemini-4-hydrogen"}`)
	check(false, 0)

	setCatalog(canonical + `,"gemini-4-hydrogen-tiered-high":{"id":"gemini-4-hydrogen-tiered-high","cost":{"input":11}}`)
	if price := Pricer()(logRecord(local)); price == nil || price.Input != 11 {
		t.Errorf("exact catalog price = %+v, want input $11", price)
	}
	if err := settings.Save(settings.Settings{ModelPrices: map[string]settings.ModelPrice{
		provider.AnyPriceKey(alias): settings.StatedPrice(catalog.Price{}),
	}}); err != nil {
		t.Fatal(err)
	}
	if price := Pricer()(logRecord(local)); price == nil || price.Input != 0 {
		t.Errorf("explicit price = %+v, want input $0", price)
	}
}
