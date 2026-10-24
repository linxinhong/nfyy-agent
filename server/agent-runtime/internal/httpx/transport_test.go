package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"go.opentelemetry.io/otel/attribute"

	"github.com/linxinhong/xingding-agent-runtime/internal/metrictest"
)

func TestProviderRequestMetrics(t *testing.T) {
	metrictest.Install()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer srv.Close()
	host, _ := url.Parse(srv.URL)
	server := attribute.String("server.address", host.Hostname())
	before2xx := metrictest.Value(t, "provider.http.duration", server, attribute.String("status_class", "2xx"))
	before4xx := metrictest.Value(t, "provider.http.duration", server, attribute.String("status_class", "4xx"))
	client := Client(0)
	for _, path := range []string{"/ok", "/fail"} {
		resp, err := client.Get(srv.URL + path + "?api_key=secret")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if metrictest.Value(t, "provider.http.duration", server, attribute.String("status_class", "2xx"))-before2xx != 1 ||
		metrictest.Value(t, "provider.http.duration", server, attribute.String("status_class", "4xx"))-before4xx != 1 {
		t.Fatal("provider request durations not recorded by status class")
	}
}
