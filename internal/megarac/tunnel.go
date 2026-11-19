package megarac

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// maxTunnelReply bounds the BMC's text reply to the tunnel request.
const maxTunnelReply = 512

// openTunnel asks the BMC's web port to carry the KVM, as JViewer's single-port mode does, and
// reads the reply from r (which must buffer w's connection). After it returns, the connection
// carries IVTP.
func openTunnel(w io.Writer, r *bufio.Reader, host string, webPort int, cookie string) error {
	if strings.ContainsAny(cookie, "\r\n ") || strings.ContainsAny(host, "\r\n ") {
		return errors.New("MegaRAC tunnel: session cookie or host contains spaces or line breaks")
	}
	// The odd "\n cookie" is what JViewer sends.
	req := fmt.Sprintf("CONNECT %s:%d HTTPS/1.1\n cookie %s\r\n\r\n", host, webPort, cookie) +
		fmt.Sprintf("JVIEWER VIDEO cookie %s\r\n\r\n", cookie)
	if _, err := io.WriteString(w, req); err != nil {
		return fmt.Errorf("MegaRAC tunnel request: %w", err)
	}
	var reply strings.Builder
	for !strings.HasSuffix(reply.String(), "\r\n\r\n") {
		b, err := r.ReadByte()
		if err != nil {
			return fmt.Errorf("MegaRAC tunnel reply: %w", err)
		}
		if reply.Len() >= maxTunnelReply {
			return errors.New("MegaRAC tunnel reply is too long")
		}
		reply.WriteByte(b)
	}
	text := strings.TrimSpace(reply.String())
	if strings.Contains(text, "ERROR") || !strings.HasPrefix(text, "HTTP/1.") || !strings.Contains(strings.SplitN(text, "\r\n", 2)[0], " 200") {
		return fmt.Errorf("MegaRAC tunnel refused: %q", firstLine(text))
	}
	return nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\r\n")
	return line
}

// disconnectLine ends the tunnelled session, as JViewer does when it closes.
func disconnectLine(cookie string) string {
	return "JVIEWER DISCONNECT Cookie " + cookie + "\r\n\r\n"
}
