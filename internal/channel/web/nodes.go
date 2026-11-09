package web

import "net/http"

// NodeEndpoints are the device-node handlers (internal/protocols/nodes). Public is
// served at /nodes and /nodes/...; every request there carries a pairing
// code or a node's own secret, so it sits outside the API token, like the
// webhook channels under /channels. Admin is the dashboard's /api/nodes,
// behind the token.
type NodeEndpoints struct {
	Public http.Handler
	Admin  http.Handler
}

func (s *Server) hasNodes() bool { return s.Nodes != nil && s.Nodes.Public != nil }

// nodeAPIRoutes mounts the dashboard's node API.
func (s *Server) nodeAPIRoutes(mux *http.ServeMux) {
	if s.Nodes == nil || s.Nodes.Admin == nil {
		return
	}
	h := s.auth(dashGuard(s.Nodes.Admin.ServeHTTP))
	mux.HandleFunc("GET /api/nodes", h)
	mux.HandleFunc("POST /api/nodes/pair", h)
	mux.HandleFunc("DELETE /api/nodes/{name}", h)
}

// nodePublicRoutes mounts the endpoints devices connect to.
func (s *Server) nodePublicRoutes(root *http.ServeMux) {
	if !s.hasNodes() {
		return
	}
	pub := endpointHeaders(s.Nodes.Public)
	// Devices send no Origin; a web page must not pair or connect.
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if why := s.foreign(r); why != "" {
			httpError(w, http.StatusForbidden, why)
			return
		}
		pub.ServeHTTP(w, r)
	})
	root.Handle("/nodes", h)
	root.Handle("/nodes/", h)
}
