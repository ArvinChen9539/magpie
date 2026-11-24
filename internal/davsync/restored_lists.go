package davsync

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/backup"
	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
)

type restoredLists map[string]provider.Provider

const restoredListsBudget = 12 * time.Second

var restoredListClient = provider.Keyed(gateway.PublicClient())

func (lists restoredLists) bring(bundle backup.Bundle, part string) error {
	keys := bundle.Keys
	if bundle.ProvidersKeys != nil {
		keys = *bundle.ProvidersKeys
	}
	if part != "providers" || !keys || len(bundle.Providers) == 0 {
		return bring(bundle, part)
	}
	before, err := provider.Stored()
	if err != nil {
		return err
	}
	if err := bring(bundle, part); err != nil {
		return err
	}
	prior := make(map[string]provider.Provider, len(before))
	for _, old := range before {
		prior[old.ID] = old
	}
	for _, incoming := range syncedProviders(bundle.Providers) {
		old := prior[incoming.ID]
		if restoredListURL(incoming) == "" || old.ThisDevice {
			continue
		}
		incoming.IconURL = ""
		if !reflect.DeepEqual(incoming, syncedProviders([]provider.Provider{old})[0]) {
			lists[incoming.ID] = incoming
		}
	}
	return nil
}

func restoredListURL(p provider.Provider) string {
	if p.ThisDevice || p.Off || p.ListsOff() || p.NoModels || p.BestInFamily || len(p.Models) > 0 || len(p.Delisted) > 0 ||
		strings.EqualFold(strings.TrimSpace(p.RefreshEvery), "off") {
		return ""
	}
	if p.Preset != "" || p.Account != nil || strings.TrimSpace(p.Key) == "" || len(p.KeysOn()) != 1 ||
		p.Anthropic != "" || p.Gemini != "" || p.Decide != "" ||
		(p.KeyProtocol != "" && p.KeyProtocol != provider.Chat && p.KeyProtocol != provider.Responses) {
		return ""
	}
	if len(p.Headers) > 0 || p.Proxy != "" || len(p.AccountProxies) > 0 || p.ClientCert != "" || p.ClientKey != "" ||
		p.ServerCA != "" || p.ServerPin != "" || p.InsecureTLS || len(p.Resolve) > 0 {
		return ""
	}
	chat := strings.TrimRight(strings.TrimSpace(p.Chat), "/")
	responses := strings.TrimRight(strings.TrimSpace(p.Responses), "/")
	if (chat == "" && responses == "") || (chat != "" && responses != "" && chat != responses) {
		return ""
	}
	base := chat
	if base == "" {
		base = responses
	}
	list := strings.TrimSpace(p.ModelsURL)
	if list == "" {
		baseURL, err := url.Parse(base)
		if err != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
			return ""
		}
		list = base + "/models"
		if !catalog.Versioned(base) {
			list = base + "/v1/models"
		}
	}
	listURL, err := url.Parse(list)
	if err != nil || listURL.Hostname() == "" || listURL.User != nil || listURL.Fragment != "" ||
		(listURL.Scheme != "https" && listURL.Scheme != "http") {
		return ""
	}
	if p.ModelsURL != "" {
		baseURL, err := url.Parse(base)
		if err != nil || !provider.SameOrigin(baseURL.Scheme, baseURL.Host, listURL.Scheme, listURL.Host) {
			return ""
		}
	}
	return list
}

func (lists restoredLists) fetch(ctx context.Context) {
	if len(lists) == 0 || ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, restoredListsBudget)
	defer cancel()
	stored, err := provider.Stored()
	if err != nil {
		return
	}
	for _, current := range syncedProviders(stored) {
		if ctx.Err() != nil {
			return
		}
		incoming, ok := lists[current.ID]
		if !ok || !reflect.DeepEqual(incoming, current) {
			continue
		}
		if !stillNeedsList(current) {
			continue
		}
		fetchCtx, finish := context.WithTimeout(ctx, 4*time.Second)
		list := restoredListURL(current)
		models := readRestoredList(fetchCtx, list, strings.TrimSpace(current.Key))
		finish()
		if len(models) > 0 && ctx.Err() == nil && stillNeedsList(current) {
			_ = catalog.SaveLive(current.ID, list, models)
		}
	}
}

func stillNeedsList(expected provider.Provider) bool {
	if _, err := os.Stat(catalog.LivePath(expected.ID)); !os.IsNotExist(err) {
		return false
	}
	stored, err := provider.Stored()
	return err == nil && slices.ContainsFunc(syncedProviders(stored), func(current provider.Provider) bool {
		return current.ID == expected.ID && reflect.DeepEqual(current, expected)
	})
}

func readRestoredList(ctx context.Context, list, key string) []catalog.Model {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, list, nil)
	if err != nil {
		return nil
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := restoredListClient.Do(request)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil
	}
	var page struct {
		HasMore bool `json:"has_more"`
		Data    []struct {
			ID          string   `json:"id"`
			Name        string   `json:"name"`
			DisplayName string   `json:"display_name"`
			Kind        string   `json:"kind"`
			Domain      string   `json:"domain"`
			Context     int      `json:"context_length"`
			Output      int      `json:"max_output_tokens"`
			Endpoints   []string `json:"supported_endpoints"`
		} `json:"data"`
	}
	const maxList = 8 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxList+1))
	if err != nil || len(body) > maxList || json.Unmarshal(body, &page) != nil || page.HasMore {
		return nil
	}
	models := make([]catalog.Model, 0, len(page.Data))
	for _, entry := range page.Data {
		if entry.ID == "" || (entry.Kind != "" && entry.Kind != "chat") || (entry.Domain != "" && entry.Domain != "LLM") {
			return nil
		}
		name := entry.DisplayName
		if name == "" {
			name = entry.Name
		}
		models = append(models, catalog.Model{ID: entry.ID, Name: name, Context: entry.Context, Output: entry.Output, APIs: catalog.EndpointAPIs(entry.Endpoints)})
	}
	return models
}
