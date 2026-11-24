package davsync

import (
	"fmt"
	"net/http"
	"net/netip"
	"testing"

	"github.com/yetone/magpie/internal/testenv"
)

// The tests run in a home of their own, never the user's real files.
func TestMain(m *testing.M) {
	testenv.Offline()
	restoredListClient.Transport = loopbackListTransport{restoredListClient.Transport}
	testenv.Main(m)
}

type loopbackListTransport struct{ http.RoundTripper }

func (transport loopbackListTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	address, err := netip.ParseAddr(request.URL.Hostname())
	if err != nil || !address.IsLoopback() {
		return nil, fmt.Errorf("test model list request to %s is outside loopback", request.URL.Host)
	}
	return transport.RoundTripper.RoundTrip(request)
}
