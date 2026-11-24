package davsync

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func TestRestoredVendorLists(t *testing.T) {
	ctx := context.Background()
	source := newComputer(t)
	source.use(t)
	fake := newGithubFixture(t)
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("restored provider's model list followed a cross-origin redirect")
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(redirectTarget.Close)
	unapproved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("restored provider contacted a private address without local permission")
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(unapproved.Close)
	var fetched, failed atomic.Int32
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Errorf("unexpected model list request for %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/live/v1/models":
			fetched.Add(1)
			active, err := provider.Stored()
			if err != nil || !slices.ContainsFunc(active, func(p provider.Provider) bool { return p.ID == "live" }) {
				t.Errorf("vendor asked before its config was active: %v", err)
			}
			if mu.TryLock() {
				mu.Unlock()
			} else {
				t.Error("vendor asked while the sync mutex was held")
			}
			lockCtx, cancel := context.WithTimeout(r.Context(), time.Second)
			defer cancel()
			if unlock, err := lock(lockCtx); err != nil {
				t.Errorf("vendor asked while the sync file lock was held: %v", err)
			} else {
				unlock()
			}
			fmt.Fprint(w, `{"object":"list","data":[{"id":"fixture-from-vendor","object":"model"}]}`)
		case "/failed/v1/models":
			failed.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
		case "/redirect/v1/models":
			http.Redirect(w, r, redirectTarget.URL+"/v1/models", http.StatusFound)
		default:
			t.Errorf("unexpected list request for %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(vendor.Close)
	for _, p := range []provider.Provider{
		{ID: "live"}, {ID: "failed"}, {ID: "cached"}, {ID: "redirect"},
		{ID: "off", Off: true}, {ID: "list-off", ListOff: true},
		{ID: "manual", RefreshEvery: "off"}, {ID: "picked", Models: []string{"by-hand"}},
		{ID: "usage-only", NoModels: true},
	} {
		p.Name, p.Chat, p.Key = p.ID, vendor.URL+"/"+p.ID+"/v1", "fixture-key"
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := provider.Save(provider.Provider{ID: "private", Name: "Private", Chat: unapproved.URL + "/v1", Key: "private-fixture-key"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.Restore([]provider.Provider{{ID: "needs-key", Name: "needs-key", Chat: "https://needs-key.invalid/v1", ModelsURL: vendor.URL + "/needs-key/models"}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("live", vendor.URL+"/live/v1", []catalog.Model{{ID: "fixture-from-vendor"}}); err != nil {
		t.Fatal(err)
	}
	cfg := githubConfig()
	if err := Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if err := Now(ctx); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"join", "restore"} {
		t.Run(mode, func(t *testing.T) {
			newComputer(t).use(t)
			local := settings.Load()
			local.FetchPrivate = []string{strings.TrimPrefix(vendor.URL, "http://"), strings.TrimPrefix(redirectTarget.URL, "http://")}
			if err := settings.Save(local); err != nil {
				t.Fatal(err)
			}
			if mode == "restore" {
				if err := provider.Save(provider.Provider{ID: "live", Name: "Live", Chat: vendor.URL + "/live/v1", Key: "previous-fixture-key", Models: []string{"previous-pick"}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := Configure(cfg); err != nil {
				t.Fatal(err)
			}
			if err := catalog.SaveLive("cached", vendor.URL+"/cached/v1", []catalog.Model{{ID: "already-here"}}); err != nil {
				t.Fatal(err)
			}
			if _, _, ok := catalog.Live("live"); ok {
				t.Fatal("receiving computer already has the vendor list")
			}
			fetched.Store(0)
			failed.Store(0)
			if _, err := Preview(ctx); err != nil {
				t.Fatal(err)
			}
			if fetched.Load() != 0 || failed.Load() != 0 {
				t.Fatal("preview contacted a vendor")
			}
			if mode == "join" {
				if err := SyncNow(ctx); err != nil {
					t.Fatal(err)
				}
			} else if parts, err := Restore(ctx); err != nil || !slices.Contains(parts, "providers") {
				t.Fatalf("restore brought %v: %v", parts, err)
			}
			p, err := provider.Find("live")
			if err != nil {
				t.Fatal(err)
			}
			if models := p.Exposed(); len(models) != 1 || models[0].ID != "fixture-from-vendor" {
				t.Fatalf("restored vendor list exposes %v", models)
			}
			if got, misses := fetched.Load(), failed.Load(); got != 1 || misses == 0 {
				t.Fatalf("list requests after restore: live=%d failed=%d", got, misses)
			}
			misses, writes := failed.Load(), fake.writes
			if err := Now(ctx); err != nil {
				t.Fatal(err)
			}
			if fetched.Load() != 1 || failed.Load() != misses || fake.writes != writes {
				t.Fatal("unchanged sync fetched lists again or pushed the local cache")
			}
		})
	}
}
