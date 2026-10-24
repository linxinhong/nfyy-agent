package blob

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
)

// fakeClamd answers one INSTREAM session, flagging streams containing "EICAR".
func fakeClamd(t *testing.T, reply func(stream []byte) string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		cmd := make([]byte, len("zINSTREAM\x00"))
		if _, err := io.ReadFull(conn, cmd); err != nil || string(cmd) != "zINSTREAM\x00" {
			return
		}
		var stream bytes.Buffer
		for {
			var size [4]byte
			if _, err := io.ReadFull(conn, size[:]); err != nil {
				return
			}
			n := binary.BigEndian.Uint32(size[:])
			if n == 0 {
				break
			}
			if _, err := io.CopyN(&stream, conn, int64(n)); err != nil {
				return
			}
		}
		_, _ = conn.Write([]byte(reply(stream.Bytes()) + "\x00"))
	}()
	return ln.Addr().String()
}

func TestClamdVerdicts(t *testing.T) {
	answer := func(stream []byte) string {
		if bytes.Contains(stream, []byte("EICAR")) {
			return "stream: Eicar-Test-Signature FOUND"
		}
		return "stream: OK"
	}
	big := strings.Repeat("x", 200<<10) // several chunks
	v, err := Clamd{Addr: fakeClamd(t, answer)}.Scan(context.Background(), strings.NewReader(big))
	if err != nil || !v.Clean {
		t.Fatalf("clean stream: %+v %v", v, err)
	}
	v, err = Clamd{Addr: fakeClamd(t, answer)}.Scan(context.Background(), strings.NewReader(big+"EICAR"))
	if err != nil || v.Clean || v.Signature != "Eicar-Test-Signature" {
		t.Fatalf("infected stream: %+v %v", v, err)
	}
	limit := func([]byte) string { return "INSTREAM size limit exceeded. ERROR" }
	if _, err := (Clamd{Addr: fakeClamd(t, limit)}).Scan(context.Background(), strings.NewReader("x")); err == nil {
		t.Fatal("a clamd error must never read as clean")
	}
	if _, err := (Clamd{Addr: "127.0.0.1:1"}).Scan(context.Background(), strings.NewReader("x")); err == nil {
		t.Fatal("an unreachable clamd must fail the scan")
	}
}
