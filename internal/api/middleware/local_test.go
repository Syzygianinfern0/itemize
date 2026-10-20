package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLocalOnly(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := LocalOnly([]string{"http://localhost:5173"})(ok)

	cases := []struct {
		name, method, host, origin string
		header                     bool
		want                       int
	}{
		{"local read", "GET", "127.0.0.1:8085", "", false, 200},
		{"localhost read", "GET", "localhost:8085", "", false, 200},
		{"ipv6 loopback", "GET", "[::1]:8085", "", false, 200},
		{"rebinding host", "GET", "evil.example:8085", "", false, 403},
		{"lan address", "GET", "192.168.1.5:8085", "", false, 403},
		{"post without header", "POST", "127.0.0.1:8085", "", false, 403},
		{"post from own UI", "POST", "127.0.0.1:8085", "http://127.0.0.1:8085", true, 200},
		{"post from dev origin", "POST", "127.0.0.1:8085", "http://localhost:5173", true, 200},
		{"post from other site", "POST", "127.0.0.1:8085", "https://evil.example", true, 403},
		{"post without origin (curl)", "PUT", "127.0.0.1:8085", "", true, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/x", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.header {
				req.Header.Set(RequestHeader, "1")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}
