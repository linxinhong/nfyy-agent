// Package journaltest is a behavioral contract for stream.Journal
// implementations.
package journaltest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/stream"
)

func frames(from, to uint64) []stream.Frame {
	var out []stream.Frame
	for seq := from; seq <= to; seq++ {
		out = append(out, stream.Frame{Seq: seq, Event: "message", Data: json.RawMessage(fmt.Sprintf(`{"n":%d}`, seq))})
	}
	return out
}

// Run exercises j; runPrefix keeps run IDs unique across shared databases.
func Run(t *testing.T, j stream.Journal, runPrefix string) {
	t.Helper()
	ctx := context.Background()
	run := runPrefix + "-run"
	alice := stream.Owner{Subject: "alice", Isolated: true, Namespace: "ns", Tenant: true}
	mallory := stream.Owner{Subject: "mallory", Isolated: true, Namespace: "ns", Tenant: true}

	if seq, err := j.LastSeq(ctx, run); err != nil || seq != 0 {
		t.Fatalf("LastSeq(unknown) = %d, %v", seq, err)
	}
	if err := j.Append(ctx, run, alice, frames(1, 3), false); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, done, err := j.Read(ctx, run, alice, 1, 0)
	if err != nil || done || len(got) != 2 || got[0].Seq != 2 || string(got[1].Data) != `{"n":3}` {
		t.Fatalf("Read = %+v, %v, %v", got, done, err)
	}
	if _, _, err := j.Read(ctx, run, mallory, 0, 0); !errors.Is(err, stream.ErrJournalNotFound) {
		t.Fatalf("Read by another owner = %v", err)
	}
	if err := j.Append(ctx, run, mallory, frames(4, 4), false); !errors.Is(err, stream.ErrJournalNotFound) {
		t.Fatalf("Append by another owner = %v", err)
	}
	if _, _, err := j.Read(ctx, runPrefix+"-missing", alice, 0, 0); !errors.Is(err, stream.ErrJournalNotFound) {
		t.Fatalf("Read(missing) = %v", err)
	}
	if got, _, _ := j.Read(ctx, run, alice, 0, 2); len(got) != 2 {
		t.Fatalf("limit ignored: %d frames", len(got))
	}

	if err := j.Append(ctx, run, alice, frames(4, 2503), true); err != nil {
		t.Fatalf("large Append: %v", err)
	}
	if seq, _ := j.LastSeq(ctx, run); seq != 2503 {
		t.Fatalf("LastSeq = %d", seq)
	}
	all, done, err := j.Read(ctx, run, alice, 0, 5000)
	if err != nil || !done || len(all) != 2503 || all[2502].Seq != 2503 {
		t.Fatalf("Read all = %d frames done=%v err=%v", len(all), done, err)
	}

	if seen, _ := j.SubscriberSeen(ctx, run); !seen.IsZero() {
		t.Fatalf("seen before any touch: %s", seen)
	}
	if err := j.TouchSubscriber(ctx, run); err != nil {
		t.Fatal(err)
	}
	if seen, err := j.SubscriberSeen(ctx, run); err != nil || time.Since(seen) > time.Minute {
		t.Fatalf("SubscriberSeen = %s, %v", seen, err)
	}

	// A resumed run reopens its journal.
	if err := j.Append(ctx, run, alice, frames(2504, 2504), false); err != nil {
		t.Fatal(err)
	}
	if _, done, _ := j.Read(ctx, run, alice, 2503, 0); done {
		t.Fatal("reopened run still reported done")
	}
	if err := j.Append(ctx, run, alice, nil, true); err != nil {
		t.Fatal(err)
	}
	if n, err := j.DeleteFinishedBefore(ctx, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	} else if _, _, err := j.Read(ctx, run, alice, 0, 1); err != nil {
		t.Fatalf("recently finished run deleted (%d): %v", n, err)
	}
	if _, err := j.DeleteFinishedBefore(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.Read(ctx, run, alice, 0, 1); !errors.Is(err, stream.ErrJournalNotFound) {
		t.Fatalf("expired run still readable: %v", err)
	}
}
