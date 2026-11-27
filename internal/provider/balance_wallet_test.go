package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBalanceWalletIdentityAcrossProviders(t *testing.T) {
	probeSandbox(t)
	var fail atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case newAPIKeyUsage:
			if fail.Load() {
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
				return
			}
			io.WriteString(w, `{"code":true,"data":{"object":"token_usage","total_available":2000000,"unlimited_quota":false,"user_quota":{"total_available":2000000,"total_used":0}}}`)
		case newAPIUserSelf, "/other" + newAPIUserSelf:
			id := 11
			if r.Header.Get("New-Api-User") == "12" {
				id = 12
			}
			fmt.Fprintf(w, `{"success":true,"data":{"id":%d,"quota":2000000}}`, id)
		case "/custom":
			io.WriteString(w, `{"data":[{"available":4}]}`)
		case sub2APIProfile:
			io.WriteString(w, `{"code":0,"data":{"id":11,"balance":4}}`)
		case sub2APIKeyUsage:
			if r.Header.Get("Authorization") == "Bearer sk-subscription" {
				io.WriteString(w, `{"mode":"unrestricted","isValid":true,"remaining":4,"unit":"USD","subscription":{"weekly_limit_usd":4}}`)
				return
			}
			if r.Header.Get("Authorization") == "Bearer sk-sub-wallet" {
				io.WriteString(w, `{"mode":"unrestricted","isValid":true,"remaining":4,"unit":"USD","balance":4}`)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	for _, fixture := range []struct{ name, endpoint, path, user string }{
		{"auto-one", newAPIUserSelf, "", "11"}, {"auto-two", newAPIUserSelf, "", "11"},
		{"other-account", newAPIUserSelf, "", "12"}, {"other-deployment", "/other" + newAPIUserSelf, newAPIQuotaPath, "11"},
		{"manual-one", "", newAPIUserQuotaPath, ""}, {"manual-two", newAPIKeyUsage, newAPIUserQuotaPath, ""},
		{"key-quota", newAPIKeyUsage, newAPIKeyPath, ""}, {"unknown", newAPIKeyUsage, newAPIUserQuotaPath, ""},
		{"custom-one", "/custom", "$data.0.available", ""}, {"custom-two", "/custom", "$data.0.available", ""},
		{"sub-profile", sub2APIProfile, "", "11"}, {"sub-profile-two", sub2APIProfile, "", "11"},
		{"sub-wallet", sub2APIKeyUsage, "", ""}, {"subscription", sub2APIKeyUsage, "", ""},
	} {
		p := Provider{ID: fixture.name, Name: fixture.name, Chat: upstream.URL + "/v1", Key: "sk-" + fixture.name, BalancePath: fixture.path}
		if fixture.endpoint != "" {
			p.BalanceURL = upstream.URL + fixture.endpoint
		}
		if fixture.user != "" {
			p.Headers = map[string]string{"New-Api-User": fixture.user}
			p.BalanceToken = "token-" + fixture.name
		}
		if err := Save(p); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"manual-one", "manual-two", "key-quota", "custom-one", "custom-two", "sub-wallet", "subscription"} {
		if err := SetKeyBalanceWallet(name, KeyID("sk-"+name), "Home wallet"); err != nil {
			t.Fatal(err)
		}
	}
	read := func() map[string]Quota {
		t.Helper()
		cards := map[string]Quota{}
		for _, card := range quotaReport(nil, nil, KeyBalances(context.Background()), time.Now()) {
			cards[card.Provider] = card
		}
		return cards
	}
	cards := read()
	for _, pair := range [][2]string{{"auto-one", "auto-two"}, {"manual-one", "manual-two"}, {"custom-one", "custom-two"}, {"sub-profile", "sub-profile-two"}} {
		left, right := cards[pair[0]], cards[pair[1]]
		if left.Balance != "$4.00" || right.Balance != "$4.00" || left.BalanceWallet == "" || left.BalanceWallet != right.BalanceWallet {
			t.Fatalf("same wallet, different grouping: %s = %q (%q, %s); %s = %q (%q, %s)", pair[0], left.BalanceWallet, left.Balance, left.Error, pair[1], right.BalanceWallet, right.Balance, right.Error)
		}
	}
	for _, name := range []string{"other-account", "other-deployment", "sub-profile", "sub-wallet"} {
		if cards[name].BalanceWallet == "" || cards[name].BalanceWallet == cards["auto-one"].BalanceWallet {
			t.Fatalf("different wallet under %s: %q", name, cards[name].BalanceWallet)
		}
	}
	for _, name := range []string{"unknown", "key-quota", "subscription"} {
		if cards[name].Balance != "$4.00" || cards[name].BalanceWallet != "" {
			t.Fatalf("%s gained a wallet identity", name)
		}
	}
	fail.Store(true)
	RefreshUsage(context.Background(), "manual-two", "")
	if card := read()["manual-two"]; card.AsOf == nil || card.BalanceWallet != cards["manual-one"].BalanceWallet {
		t.Fatalf("failed refresh lost its wallet: %q, %v", card.BalanceWallet, card.AsOf)
	}
	if err := SetKeyBalanceWallet("manual-two", KeyID("sk-manual-two"), ""); err != nil {
		t.Fatal(err)
	}
	if card := read()["manual-two"]; card.BalanceWallet != "" {
		t.Fatalf("failed refresh recovered a removed binding: %q", card.BalanceWallet)
	}
}
