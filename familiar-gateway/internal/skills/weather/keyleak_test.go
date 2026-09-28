package weather

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The API key is part of the request URL's path, and a transport error
// quotes the URL: returned as-is, it reached the model, the tool
// transcript, the logs and the Home widget's error body.
func TestFetchPirate_ErrorsNeverCarryTheKey(t *testing.T) {
	const key = "SECRETKEY123"
	for name, rt := range map[string]roundTripFunc{
		"transport error": func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp: i/o timeout")
		},
		"error status echoing the url": func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 502, Body: io.NopCloser(strings.NewReader("bad gateway for " + r.URL.String())), Header: http.Header{}}, nil
		},
	} {
		s := New(&http.Client{Transport: rt}, key)
		_, err := s.fetchPirate(context.Background(), 45.4, -122.3, "")
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		if strings.Contains(err.Error(), key) {
			t.Errorf("%s: error carries the API key: %v", name, err)
		}
		if name == "transport error" && strings.Contains(err.Error(), "/forecast/") {
			t.Errorf("transport error still quotes the request URL: %v", err)
		}
	}
}
