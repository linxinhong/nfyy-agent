// Package httpx holds the HTTP transport shared by model provider clients.
package httpx

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/linxinhong/xingding-agent-runtime/pkg/observability"
)

// Transport is shared by every model client so connections to a provider
// are pooled across calls, models, and agents. Go's default keeps only two
// idle connections per host, which under concurrent runs against one
// provider means a new TCP+TLS handshake for most requests.
var Transport http.RoundTripper = &instrumented{base: newTransport()}

var requestDuration, _ = observability.Meter("xingding.providers").Float64Histogram("provider.http.duration",
	metric.WithUnit("s"), metric.WithDescription("Time from request to response headers, by host and status class"))

// instrumented records provider request latency. Only the host and status
// class are recorded, never paths, queries, or headers.
type instrumented struct{ base http.RoundTripper }

func (t *instrumented) RoundTrip(req *http.Request) (*http.Response, error) {
	started := time.Now()
	resp, err := t.base.RoundTrip(req)
	class := "error"
	if err == nil {
		class = strconv.Itoa(resp.StatusCode/100) + "xx"
	}
	requestDuration.Record(context.WithoutCancel(req.Context()), time.Since(started).Seconds(), metric.WithAttributes(
		attribute.String("server.address", req.URL.Hostname()), attribute.String("status_class", class)))
	return resp, err
}

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// Client returns a client on the shared transport with a total timeout;
// zero means none (streams use idle timeouts instead).
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Transport: Transport, Timeout: timeout}
}
