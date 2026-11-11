package webpush

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
)

// DialOnly connects every request to srv, whatever host it names, so
// subscriptions can keep real push service names in tests.
func DialOnly(srv *httptest.Server) *http.Client {
	addr := srv.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // the test server's own certificate
	}}
}
