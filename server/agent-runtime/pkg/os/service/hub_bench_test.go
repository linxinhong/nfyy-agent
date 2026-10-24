package service

import (
	"fmt"
	"sync/atomic"
	"testing"
)

// BenchmarkHubPublishParallel models many concurrent streamed runs, each
// publishing token deltas into its own journal.
func BenchmarkHubPublishParallel(b *testing.B) {
	h := NewStreamHub(StreamHubConfig{})
	const runs = 64
	pubs := make([]*Publisher, runs)
	for i := range pubs {
		pubs[i] = h.Start(fmt.Sprintf("run-%d", i), StreamOwner{}, func() {})
	}
	var next atomic.Int64
	delta := map[string]string{"content": "token"}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		pub := pubs[next.Add(1)%runs]
		for pb.Next() {
			pub.Publish("message", delta)
		}
	})
}

// BenchmarkHubStartWithRetainedJournals measures starting a stream while
// many finished journals are retained for reconnects.
func BenchmarkHubStartWithRetainedJournals(b *testing.B) {
	h := NewStreamHub(StreamHubConfig{MaxRuns: 20000})
	for i := 0; i < 10000; i++ {
		pub := h.Start(fmt.Sprintf("retained-%d", i), StreamOwner{}, func() {})
		pub.Publish("run_completed", nil)
		pub.Close()
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pub := h.Start(fmt.Sprintf("run-%d", i%5000), StreamOwner{}, func() {})
		pub.Close()
	}
}
