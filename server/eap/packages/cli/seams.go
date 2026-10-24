package cli

// Small stdlib indirections so the CLI file bodies stay readable and the
// http/time/os seams are obvious.

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	httpGet    = http.MethodGet
	httpPost   = http.MethodPost
	timeSecond = time.Second
	timeMinute = time.Minute
)

var httpClient = func() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}

var httpNewRequestWithContext = http.NewRequestWithContext

var osMkdirAll = os.MkdirAll

var osWriteFile = os.WriteFile

var timeNow = time.Now

func sleepContext(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func copyIO(dst *strings.Builder, src io.Reader) (int64, error) {
	n, err := io.Copy(dst, src)
	return n, err
}
