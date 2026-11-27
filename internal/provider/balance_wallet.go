package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

func walletDigest(values ...any) string {
	encoded, _ := json.Marshal(values)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:16])
}

func walletConfigStamp(p Provider) string {
	return walletDigest(p.BalanceURL, p.BalancePath, p.BalanceMethod, p.NoBalanceProbe, p.Chat, p.Responses, p.Anthropic,
		p.BalanceToken, p.Headers, p.KeyBalanceAuth[keyID(p.Key)].Wallet, p.ProxyChoice(), p.Resolve)
}

func walletField(path string) string {
	return strings.Join(strings.Fields(path), "")
}

func newAPIWalletField(path string) bool {
	const field = "data.user_quota.total_available"
	path = strings.TrimLeft(walletField(path), "$¥€£")
	if path == field {
		return true
	}
	divisor, found := strings.CutPrefix(path, field+"/")
	scale, err := strconv.ParseFloat(divisor, 64)
	return found && err == nil && scale > 0
}

func identifyBalanceWallet(p Provider, asked, answered *url.URL, body []byte, field string, parts []BalancePart, windows []QuotaWindow) string {
	if len(windows) != 0 || len(parts) != 1 || parts[0].Percent != nil || parts[0].Label != "" ||
		asked.Scheme != answered.Scheme || !strings.EqualFold(asked.Host, answered.Host) || asked.EscapedPath() != answered.EscapedPath() {
		return ""
	}
	amount := currencyPart.FindStringSubmatch(parts[0].Text)
	if amount == nil {
		return ""
	}
	path := strings.TrimRight(asked.EscapedPath(), "/")
	configuredField := strings.TrimSpace(field)
	field = walletField(configuredField)
	scope, endpoint, canIdentify := path, "custom", false
	switch {
	case strings.HasSuffix(path, newAPIUserSelf):
		scope, endpoint = strings.TrimSuffix(path, newAPIUserSelf), "new-api"
		if field != walletField(newAPIQuotaPath) {
			return ""
		}
		canIdentify = true
	case strings.HasSuffix(path, newAPIKeyUsage):
		scope, endpoint = strings.TrimSuffix(path, newAPIKeyUsage), "new-api"
		if !newAPIWalletField(field) {
			return ""
		}
		if field == walletField(newAPIUserQuotaPath) {
			field = walletField(newAPIQuotaPath)
		}
	case strings.HasSuffix(path, sub2APIProfile):
		scope, endpoint = strings.TrimSuffix(path, sub2APIProfile), "sub2api"
		if field != "$data.balance" {
			return ""
		}
		canIdentify = true
	case strings.HasSuffix(path, sub2APIKeyUsage):
		scope, endpoint = strings.TrimSuffix(path, sub2APIKeyUsage), "sub2api"
		var reply struct {
			Mode         string          `json:"mode"`
			Balance      *float64        `json:"balance"`
			Subscription json.RawMessage `json:"subscription"`
		}
		if json.Unmarshal(body, &reply) != nil || reply.Mode != "unrestricted" || reply.Balance == nil || len(reply.Subscription) != 0 ||
			(field != "$remaining" && field != "$balance") {
			return ""
		}
		field = "$data.balance"
	default:
		field = configuredField
	}
	origin := asked.Scheme + "://" + strings.ToLower(asked.Host)
	if name := p.KeyBalanceAuth[keyID(p.Key)].Wallet; name != "" {
		return walletDigest(origin, scope, endpoint, amount[1], field, p.Resolve, "binding", name)
	}
	if canIdentify && asked.RawQuery == "" && answered.RawQuery == "" {
		var reply struct {
			Data struct {
				ID int64 `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &reply) == nil && reply.Data.ID > 0 {
			return walletDigest(origin, scope, endpoint, amount[1], p.Resolve, p.ProxyChoice(), "account", reply.Data.ID)
		}
	}
	return ""
}
