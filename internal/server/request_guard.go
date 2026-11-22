package server

import (
	"net"
	"net/http"
)

// There are no logins. The listener is loopback only (cmd/server refuses
// anything else), and two checks keep a web page in my own browser from
// driving the API, whose DB holds the keys to every upstream account:
//
//   - Host must be BASE_URL's host (what Caddy forwards for
//     https://7060.port.localhost) or this machine's loopback address on the
//     listen port. A DNS rebinding page (evil.example resolving to 127.0.0.1)
//     sends its own Host and is refused with 421, so it cannot read
//     responses.
//   - Cross-origin writes are refused (http.CrossOriginProtection:
//     Sec-Fetch-Site, else Origin vs Host). Requests with neither header,
//     such as curl and MCP clients, pass.
func (s *Server) allowRequest(w http.ResponseWriter, r *http.Request) bool {
	if !s.allowedHost(r.Host) {
		writeJSONError(w, http.StatusMisdirectedRequest, "unknown host")
		return false
	}
	if err := crossOriginProtection.Check(r); err != nil {
		writeJSONError(w, http.StatusForbidden, "cross-origin request refused")
		return false
	}
	return true
}

var crossOriginProtection = http.NewCrossOriginProtection()

func (s *Server) allowedHost(host string) bool {
	if host == s.baseHost {
		return true
	}
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	_, listenPort, err := net.SplitHostPort(s.listenAddr)
	if err != nil || port != listenPort {
		return false
	}
	return h == "localhost" || h == "127.0.0.1" || h == "[::1]" || h == "::1"
}
