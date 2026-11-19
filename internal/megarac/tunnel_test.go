package megarac

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
)

type rw struct {
	io.Reader
	io.Writer
}

func TestOpenTunnelSendsJViewerRequests(t *testing.T) {
	var out bytes.Buffer
	in := bufio.NewReader(strings.NewReader("HTTP/1.0 200 OK\r\n\r\n" + "IVTP"))
	if err := openTunnel(rw{in, &out}, in, "192.0.2.30", 443, "COOKIE123"); err != nil {
		t.Fatalf("openTunnel: %v", err)
	}
	want := "CONNECT 192.0.2.30:443 HTTPS/1.1\n cookie COOKIE123\r\n\r\n" +
		"JVIEWER VIDEO cookie COOKIE123\r\n\r\n"
	if out.String() != want {
		t.Errorf("sent %q, want %q", out.String(), want)
	}
	rest, _ := io.ReadAll(in)
	if string(rest) != "IVTP" {
		t.Errorf("tunnel reply consumed %q of the IVTP stream", "IVTP"[:4-len(rest)])
	}
}

func TestOpenTunnelReportsErrors(t *testing.T) {
	for _, reply := range []string{"ERROR:4\\ invalid session\r\n\r\n", "HTTP/1.0 403 Forbidden\r\n\r\n", ""} {
		in := bufio.NewReader(strings.NewReader(reply))
		if err := openTunnel(rw{in, io.Discard}, in, "192.0.2.30", 443, "c"); err == nil {
			t.Errorf("reply %q: want an error", reply)
		}
	}
}

func TestOpenTunnelRejectsCookieInjection(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("HTTP/1.0 200 OK\r\n\r\n"))
	if err := openTunnel(rw{in, io.Discard}, in, "192.0.2.30", 443, "a\r\nJVIEWER DISCONNECT"); err == nil {
		t.Fatal("want an error for a cookie with line breaks")
	}
}

func TestDisconnectLine(t *testing.T) {
	if got := disconnectLine("C1"); got != "JVIEWER DISCONNECT Cookie C1\r\n\r\n" {
		t.Errorf("disconnectLine = %q", got)
	}
}
