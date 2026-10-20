package middleware

import (
	"net"
	"net/http"
	"strings"
)

// RequestHeader must be sent on every state-changing request. Browsers only
// send custom headers cross-origin after a CORS preflight, which other sites
// fail, so a page on another origin cannot approve or apply anything.
const RequestHeader = "X-Itemize-Request"

// LocalOnly restricts the API to the local machine and its own UI:
//   - the Host header must name a loopback address (blocks DNS rebinding);
//   - POST/PUT/PATCH/DELETE need RequestHeader, and an Origin header, if
//     present, must be this server or one of allowedOrigins (blocks CSRF).
func LocalOnly(allowedOrigins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		allowed[origin] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isLoopbackHost(r.Host) {
				http.Error(w, "forbidden: itemize only serves localhost", http.StatusForbidden)
				return
			}
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
			default:
				if r.Header.Get(RequestHeader) == "" {
					http.Error(w, "forbidden: missing "+RequestHeader+" header", http.StatusForbidden)
					return
				}
				if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host && !allowed[origin] {
					http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
