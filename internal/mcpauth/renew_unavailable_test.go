package mcpauth_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/mcpauth"
	"github.com/yetone/magpie/internal/mcpauth/mcpauthtest"
)

func TestRenewUnavailableUsesOnlyUsableAccess(t *testing.T) {
	home(t)
	clock := setClock(t)
	fake := mcpauthtest.New(t)
	fake.ExpiresIn = 10
	const name = "due"
	fake.SignIn(t, name)
	original, _ := mcpauth.Get(name)
	fake.SetRefreshStatus(http.StatusServiceUnavailable)
	for range 2 {
		if access, err := mcpauth.Token(t.Context(), name); err != nil || access != original.Access {
			t.Fatalf("usable access during outage: %q, %v", access, err)
		}
	}
	clock.add(11 * time.Second)
	for _, remaining := range []time.Duration{19 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		access, err := mcpauth.Token(t.Context(), name)
		var unavailable *mcpauth.RenewalUnavailableError
		if access != "" || !errors.As(err, &unavailable) || unavailable.Wait() != remaining {
			t.Fatalf("expired access during outage: %q, %v, want wait %s", access, err, remaining)
		}
		clock.add(remaining)
	}
	if got := fake.RefreshAsked(); got != 7 {
		t.Fatalf("renewal attempts: %d, want 7", got)
	}
	fake.SetRefreshStatus(0)
	if access, err := mcpauth.Token(t.Context(), name); err != nil || access == "" || access == original.Access {
		t.Fatalf("access after recovery: %q, %v", access, err)
	}
	if record, _ := mcpauth.Get(name); record.Dead || record.RetryAt != 0 || record.RenewFailures != 0 {
		t.Fatal("renewal did not clear the saved outage")
	}
}

type unavailableTransport func(*http.Request) (*http.Response, error)

func (transport unavailableTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestRenewAcceptsReissuedAccess(t *testing.T) {
	home(t)
	clock := setClock(t)
	fake := mcpauthtest.New(t)
	const name = "reissued"
	fake.SignIn(t, name)
	original, _ := mcpauth.Get(name)
	fake.Revoke()
	fake.SetRefreshStatus(http.StatusBadGateway)
	if _, err := mcpauth.Renew(t.Context(), name, original.Access); err == nil {
		t.Fatal("renewal unexpectedly succeeded during the outage")
	}
	clock.add(time.Minute)
	previous := mcpauth.Client
	t.Cleanup(func() { mcpauth.Client = previous })
	mcpauth.Client = &http.Client{Transport: unavailableTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != original.TokenURL {
			t.Errorf("unexpected token URL %s", request.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"access_token":%q,"expires_in":7200}`, original.Access))),
		}, nil
	})}
	if access, err := mcpauth.Token(t.Context(), name); err != nil || access != original.Access {
		t.Fatalf("successfully reissued access: %q, %v", access, err)
	}
	if record, _ := mcpauth.Get(name); record.Expires <= original.Expires || record.AccessRejected || record.Dead || record.RetryAt != 0 {
		t.Fatal("successful reissuance did not update expiry and clear the retry state")
	}
}

func TestRenewUnavailableClassifiesTransport(t *testing.T) {
	for _, test := range []struct {
		name      string
		failure   error
		cancelled bool
		wait      bool
	}{
		{name: "dns", failure: &net.DNSError{Err: "synthetic lookup failure"}, wait: true},
		{name: "connection", failure: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("synthetic refusal")}, wait: true},
		{name: "client-timeout", failure: context.DeadlineExceeded, wait: true},
		{name: "request-cancelled", failure: context.Canceled, cancelled: true},
		{name: "other-client-error", failure: errors.New("synthetic non-network error")},
	} {
		t.Run(test.name, func(t *testing.T) {
			home(t)
			setClock(t)
			fake := mcpauthtest.New(t)
			fake.SignIn(t, test.name)
			record, _ := mcpauth.Get(test.name)
			previous := mcpauth.Client
			t.Cleanup(func() { mcpauth.Client = previous })
			mcpauth.Client = &http.Client{Transport: unavailableTransport(func(request *http.Request) (*http.Response, error) {
				if request.URL.String() != record.TokenURL {
					t.Errorf("unexpected token URL %s", request.URL)
				}
				return nil, test.failure
			})}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.cancelled {
				cancel()
			}
			access, err := mcpauth.Renew(ctx, test.name, record.Access)
			var unavailable *mcpauth.RenewalUnavailableError
			if access != "" || err == nil || errors.As(err, &unavailable) != test.wait || errors.Is(err, mcpauth.ErrExpired) {
				t.Fatalf("Renew = %q, %v, want a failure with wait=%v", access, err, test.wait)
			}
			status := mcpauth.StatusOf(test.name, fake.URL)
			if !status.SignedIn || status.Dead || (status.RetryAt > 0) != test.wait {
				t.Fatalf("status after failed renewal: %+v", status)
			}
		})
	}
}
