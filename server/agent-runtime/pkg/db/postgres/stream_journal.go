package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/stream"
)

// StreamJournal returns a stream.Journal shared by every replica using the
// database; set it as AgentOSConfig.Streams.Journal.
func (d *DB) StreamJournal() stream.Journal { return &streamJournal{db: d} }

type streamJournal struct{ db *DB }

var _ stream.Journal = (*streamJournal)(nil)

// framesPerInsert keeps multi-row inserts well below PostgreSQL's
// 65535-parameter limit.
const framesPerInsert = 1000

// Append writes each chunk of frames in one statement: the run row is
// created or, for its owner only, updated with the done state, and the
// frames are inserted only when that succeeded. A flush therefore costs one
// round trip, and readers see frames and the done marker together.
func (j *streamJournal) Append(ctx context.Context, runID string, owner stream.Owner, frames []stream.Frame, done bool) error {
	for start := 0; ; start += framesPerInsert {
		end := min(start+framesPerInsert, len(frames))
		// Only the last chunk may mark the run done.
		if err := j.appendChunk(ctx, runID, owner, frames[start:end], done && end == len(frames)); err != nil {
			return err
		}
		if end == len(frames) {
			return nil
		}
	}
}

func (j *streamJournal) appendChunk(ctx context.Context, runID string, owner stream.Owner, frames []stream.Frame, done bool) error {
	var sb strings.Builder
	args := make([]any, 0, 6+3*len(frames))
	args = append(args, runID, owner.Subject, owner.Isolated, owner.Namespace, owner.Tenant, done)
	// A resumed run (done = false) reopens its journal.
	sb.WriteString(`WITH owned AS (
		INSERT INTO agent_runtime_stream_runs AS r (run_id, subject, isolated, namespace, tenant, done, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, CASE WHEN $6 THEN now() END)
		ON CONFLICT (run_id) DO UPDATE SET done = EXCLUDED.done, finished_at = EXCLUDED.finished_at
		WHERE r.subject = EXCLUDED.subject AND r.isolated = EXCLUDED.isolated
		  AND r.namespace = EXCLUDED.namespace AND r.tenant = EXCLUDED.tenant
		RETURNING run_id)`)
	if len(frames) > 0 {
		sb.WriteString(`, inserted AS (
		INSERT INTO agent_runtime_stream_frames (run_id, seq, event, data)
		SELECT owned.run_id, v.seq, v.event, v.data FROM owned, (VALUES `)
		for i, f := range frames {
			if i > 0 {
				sb.WriteString(",")
			}
			n := len(args)
			fmt.Fprintf(&sb, "($%d::bigint, $%d::text, $%d::text)", n+1, n+2, n+3)
			args = append(args, int64(f.Seq), f.Event, string(f.Data))
		}
		sb.WriteString(`) AS v(seq, event, data) ON CONFLICT (run_id, seq) DO NOTHING)`)
	}
	sb.WriteString(` SELECT count(*) FROM owned`)
	var owned int
	if err := j.db.db.QueryRowContext(ctx, sb.String(), args...).Scan(&owned); err != nil {
		return err
	}
	if owned == 0 {
		return stream.ErrJournalNotFound
	}
	return nil
}

func (j *streamJournal) Read(ctx context.Context, runID string, owner stream.Owner, after uint64, limit int) ([]stream.Frame, bool, error) {
	if limit <= 0 {
		limit = 1000
	}
	var stored stream.Owner
	var done bool
	err := j.db.db.QueryRowContext(ctx, `SELECT subject, isolated, namespace, tenant, done FROM agent_runtime_stream_runs WHERE run_id = $1`, runID).
		Scan(&stored.Subject, &stored.Isolated, &stored.Namespace, &stored.Tenant, &done)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && stored != owner) {
		return nil, false, stream.ErrJournalNotFound
	}
	if err != nil {
		return nil, false, err
	}
	rows, err := j.db.db.QueryContext(ctx, `SELECT seq, event, data FROM agent_runtime_stream_frames
		WHERE run_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`, runID, int64(after), limit)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var frames []stream.Frame
	for rows.Next() {
		var seq int64
		var f stream.Frame
		var data string
		if err := rows.Scan(&seq, &f.Event, &data); err != nil {
			return nil, false, err
		}
		f.Seq, f.Data = uint64(seq), json.RawMessage(data)
		frames = append(frames, f)
	}
	// "done" was read before the frames: a run that finished in between
	// reports not-done once more and the reader polls again.
	return frames, done, rows.Err()
}

func (j *streamJournal) LastSeq(ctx context.Context, runID string) (uint64, error) {
	var seq sql.NullInt64
	if err := j.db.db.QueryRowContext(ctx, `SELECT max(seq) FROM agent_runtime_stream_frames WHERE run_id = $1`, runID).Scan(&seq); err != nil {
		return 0, err
	}
	return uint64(seq.Int64), nil
}

func (j *streamJournal) TouchSubscriber(ctx context.Context, runID string) error {
	_, err := j.db.db.ExecContext(ctx, `UPDATE agent_runtime_stream_runs SET subscriber_seen_at = now() WHERE run_id = $1`, runID)
	return err
}

func (j *streamJournal) SubscriberSeen(ctx context.Context, runID string) (time.Time, error) {
	var seen sql.NullTime
	err := j.db.db.QueryRowContext(ctx, `SELECT subscriber_seen_at FROM agent_runtime_stream_runs WHERE run_id = $1`, runID).Scan(&seen)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	return seen.Time, err
}

func (j *streamJournal) DeleteFinishedBefore(ctx context.Context, cutoff time.Time) (int, error) {
	result, err := j.db.db.ExecContext(ctx, `DELETE FROM agent_runtime_stream_runs WHERE done AND finished_at < $1`, cutoff.UTC())
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}
