package pipeline

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/sidecar"
)

// taskRoutes routes each named sidecar task to an endpoint (model id
// "test/<task>"), satisfying both sidecar resolver interfaces.
type taskRoutes map[string]string

func (r taskRoutes) Resolve(role string) (string, int, bool) {
	if _, ok := r[role]; ok {
		return "test/" + role, 0, true
	}
	return "", 0, false
}
func (r taskRoutes) Status(string) string { return "online" }
func (r taskRoutes) Chain(role string) []string {
	if _, ok := r[role]; ok {
		return []string{"test/" + role}
	}
	return nil
}
func (r taskRoutes) EndpointForModel(id string) string {
	return r[strings.TrimPrefix(id, "test/")]
}

// fakeSidecar is an OpenAI-compatible server answering every completion
// with reply(requestBody), recording the bodies. Each arrival is also
// signalled on arrived (buffered).
type fakeSidecar struct {
	*httptest.Server
	mu      sync.Mutex
	bodies  []string
	arrived chan string
}

func newFakeSidecar(t *testing.T, reply func(body string) string) *fakeSidecar {
	t.Helper()
	f := &fakeSidecar{arrived: make(chan string, 64)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusOK)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(b))
		f.mu.Unlock()
		f.arrived <- string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": reply(string(b))},
				"finish_reason": "stop",
			}},
		})
	}))
	t.Cleanup(f.Close)
	return f
}

// sidecarFor builds a sidecar client routing the given tasks to f.
func sidecarFor(f *fakeSidecar, tasks ...string) *sidecar.Client {
	routes := taskRoutes{}
	for _, task := range tasks {
		routes[task] = f.URL
	}
	return sidecar.NewClient(config.SidecarConfig{Enabled: true}, routes, routes)
}
