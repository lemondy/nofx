package binance

import (
	"net/http"
	"net/http/httptest"
)

// memoryHTTPServer preserves the existing SDK/handler tests without binding a
// port. The transport never reaches the network, including constructor calls.
type memoryHTTPServer struct {
	URL    string
	client *http.Client
}

func newMemoryHTTPServer(handler http.Handler) *memoryHTTPServer {
	return &memoryHTTPServer{URL: "http://binance.test", client: &http.Client{Transport: batch1Transport(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		clone := r.Clone(r.Context())
		if clone.Body == nil {
			clone.Body = http.NoBody
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, clone)
		resp := w.Result()
		resp.Request = r
		return resp, nil
	})}}
}
func (s *memoryHTTPServer) Client() *http.Client { return s.client }
func (s *memoryHTTPServer) Close()               {}
