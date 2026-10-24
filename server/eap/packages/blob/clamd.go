package blob

// Malware scanning through clamd's INSTREAM command.
//
// Why clamd: ClamAV is the scanner a law firm can run on its own machines,
// and its daemon protocol is small enough to speak directly without a
// client library.
//
// Pitfalls:
//   - clamd refuses streams over its StreamMaxLength (default 25M); set it to
//     at least 50M, or larger attachments fail the scan and are refused.
//   - Any answer other than "OK" or "... FOUND" is an error, never "clean".

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Verdict is the result of one scan.
type Verdict struct {
	Clean     bool
	Signature string // set when not clean
}

// Scanner checks attachment bytes for malware.
type Scanner interface {
	Scan(ctx context.Context, r io.Reader) (Verdict, error)
}

// Clamd scans over a clamd TCP socket ("host:port").
type Clamd struct {
	Addr    string
	Timeout time.Duration
}

func (c Clamd) Scan(ctx context.Context, r io.Reader) (Verdict, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		return Verdict{}, fmt.Errorf("clamd dial: %w", err)
	}
	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return Verdict{}, fmt.Errorf("clamd command: %w", err)
	}
	buf := make([]byte, 64<<10)
	var size [4]byte
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			binary.BigEndian.PutUint32(size[:], uint32(n))
			if _, err := conn.Write(size[:]); err != nil {
				return Verdict{}, fmt.Errorf("clamd stream: %w", err)
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return Verdict{}, fmt.Errorf("clamd stream: %w", err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return Verdict{}, fmt.Errorf("read attachment: %w", readErr)
		}
	}
	binary.BigEndian.PutUint32(size[:], 0)
	if _, err := conn.Write(size[:]); err != nil {
		return Verdict{}, fmt.Errorf("clamd stream end: %w", err)
	}
	reply, err := bufio.NewReader(io.LimitReader(conn, 4096)).ReadString(0)
	if err != nil && err != io.EOF {
		return Verdict{}, fmt.Errorf("clamd reply: %w", err)
	}
	return parseClamdReply(reply)
}

// parseClamdReply reads "stream: OK" or "stream: <signature> FOUND".
func parseClamdReply(reply string) (Verdict, error) {
	reply = strings.TrimSpace(strings.TrimRight(reply, "\x00"))
	body, ok := strings.CutPrefix(reply, "stream: ")
	switch {
	case ok && body == "OK":
		return Verdict{Clean: true}, nil
	case ok && strings.HasSuffix(body, " FOUND"):
		return Verdict{Signature: strings.TrimSuffix(body, " FOUND")}, nil
	default:
		return Verdict{}, fmt.Errorf("clamd: %q", reply)
	}
}
