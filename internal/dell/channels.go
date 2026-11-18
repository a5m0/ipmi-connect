package dell

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/mhamzahkhan/ipmi-connect/internal/bmc"
	"github.com/mhamzahkhan/ipmi-connect/internal/trust"
)

// maxTokenLen is the size of each token field in the cb login packet.
const maxTokenLen = 96

// connectAPCP performs the 53-byte APCP handshake on the given port and channel mode,
// upgrading to TLS 1.0.
//
// The returned connection still carries the handshake deadline, so the caller's authentication
// exchange is bounded too; callers clear it once authenticated.
func (c *Client) connectAPCP(port int, mode byte) (net.Conn, error) {
	return c.connectAPCPVersion(port, mode, 2, 34) // the KVM viewer's version bytes
}

// apcpGreeting builds the 53-byte session request.
func apcpGreeting(mode, major, minor byte, nonce []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("APCP")
	_ = binary.Write(&buf, binary.BigEndian, uint32(53))
	_ = binary.Write(&buf, binary.BigEndian, uint16(0x0100))
	_ = binary.Write(&buf, binary.BigEndian, uint16(0x0000))
	buf.WriteByte(mode)
	buf.WriteByte(major)
	buf.WriteByte(minor)
	buf.WriteByte(0)
	_ = binary.Write(&buf, binary.BigEndian, uint32(5)) // cleartext | anonymous TLS
	buf.WriteByte(32)
	buf.Write(nonce)
	return buf.Bytes()
}

// connectAPCPVersion is connectAPCP with explicit version bytes; the virtual media client sends 0.0.
func (c *Client) connectAPCPVersion(port int, mode, major, minor byte) (net.Conn, error) {
	rawConn, err := c.ep.DialPort(context.Background(), port)
	if err != nil {
		return nil, bmc.FormatError("Dell connectAPCP dial", err)
	}
	_ = rawConn.SetDeadline(time.Now().Add(apcpHandshakeTimeout))

	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		rawConn.Close()
		return nil, bmc.FormatError("Dell connectAPCP rand.Read", err)
	}

	if _, err := rawConn.Write(apcpGreeting(mode, major, minor, nonce)); err != nil {
		rawConn.Close()
		return nil, bmc.FormatError("Dell connectAPCP write handshake", err)
	}

	serverCaps, err := readAPCPReply(rawConn)
	if err != nil {
		rawConn.Close()
		return nil, err
	}

	if serverCaps&apcpCapTLS == 0 {
		// The BMC would carry the one-time tokens, keystrokes and screen in clear text.
		if !c.ep.InsecureChannelsAllowed() {
			rawConn.Close()
			return nil, ErrPlainAPCP
		}
		c.plainChannels.Store(true)
		log.Printf("[Dell] WARNING: the BMC offers no TLS on port %d; this channel is unencrypted", port)
		return rawConn, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), apcpHandshakeTimeout)
	defer cancel()
	tlsConn, err := c.ep.TLSClient(ctx, rawConn, "kvm")
	if err != nil {
		rawConn.Close()
		return nil, bmc.FormatError("Dell APCP TLS handshake", err)
	}
	return tlsConn, nil
}

// apcpCapTLS is the capability bit for TLS in the APCP greeting.
const apcpCapTLS = 0x04

// ErrPlainAPCP means the BMC offered an APCP channel without TLS and unencrypted channels are
// not authorised for it.
var ErrPlainAPCP = errors.New("the BMC offers this KVM channel without encryption; " +
	"allow unencrypted channels for this device to connect")

// readAPCPReply reads the 53-byte greeting reply and returns the server's capabilities.
func readAPCPReply(conn net.Conn) (uint32, error) {
	resp := make([]byte, 53)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return 0, bmc.FormatError("Dell connectAPCP read response", err)
	}
	if string(resp[:4]) != "APCP" {
		return 0, fmt.Errorf("invalid APCP magic: %v", resp[:4])
	}
	return binary.BigEndian.Uint32(resp[14:18]), nil
}

// ProbeKVM learns the certificate the KVM port presents, without authenticating: the greeting,
// then a TLS handshake that is abandoned once the certificate is seen. It reports ok=false when
// the BMC offers no TLS on the port.
func ProbeKVM(ctx context.Context, ep *bmc.Endpoint, port int) (res trust.ProbeResult, tlsOffered bool, err error) {
	if err := ep.AllowPort(port); err != nil {
		return trust.ProbeResult{}, false, err
	}
	dial := func(ctx context.Context) (net.Conn, error) {
		conn, err := ep.DialPort(ctx, port)
		if err != nil {
			return nil, err
		}
		_ = conn.SetDeadline(time.Now().Add(apcpHandshakeTimeout))
		nonce := make([]byte, 32)
		_, _ = rand.Read(nonce)
		if _, err := conn.Write(apcpGreeting(channelModeControl, 2, 34, nonce)); err != nil {
			conn.Close()
			return nil, err
		}
		caps, err := readAPCPReply(conn)
		if err != nil {
			conn.Close()
			return nil, err
		}
		if caps&apcpCapTLS == 0 {
			conn.Close()
			return nil, errNoTLS
		}
		return conn, nil
	}
	res, err = trust.ProbeWithFallback(ctx, dial, ep.Host, "kvm")
	if errors.Is(err, errNoTLS) {
		return trust.ProbeResult{}, false, nil
	}
	return res, err == nil, err
}

var errNoTLS = errors.New("no TLS offered")

// ConnectControl establishes Channel Mode 3 (Control/Auth) and performs cb authentication.
func (c *Client) ConnectControl() error {
	c.ConnMu.Lock()
	defer c.ConnMu.Unlock()

	conn, err := c.connectAPCP(c.VPort, channelModeControl)
	if err != nil {
		return bmc.FormatError("Dell ConnectControl", err)
	}

	// Build cb authentication packet (ID 258, total length 217)
	cbBuf := make([]byte, controlAuthLen)
	copy(cbBuf[0:4], "BEEF")
	binary.BigEndian.PutUint16(cbBuf[4:6], pktControlAuth)
	binary.BigEndian.PutUint16(cbBuf[6:8], controlAuthLen)

	if len(c.UserTokenStr) > maxTokenLen || len(c.PassTokenStr) > maxTokenLen {
		conn.Close()
		return fmt.Errorf("KVM tokens are longer than %d bytes", maxTokenLen)
	}
	payload := cbBuf[8:]
	payload[0] = byte(len(c.UserTokenStr))
	copy(payload[1:1+len(c.UserTokenStr)], []byte(c.UserTokenStr))
	payload[97] = byte(len(c.PassTokenStr))
	copy(payload[98:98+len(c.PassTokenStr)], []byte(c.PassTokenStr))
	payload[202] = 1 // port
	payload[203] = 0 // channel
	binary.BigEndian.PutUint32(payload[204:208], c.ClientRandom)
	payload[208] = 0 // shared_session = 0

	if _, err := conn.Write(cbBuf); err != nil {
		conn.Close()
		return bmc.FormatError("Dell ConnectControl send cb packet", err)
	}

	// yb, "User Login Response": 0x83xx. The BMC may send notifications first.
	yb, err := awaitPacket(conn, func(id uint16) bool { return id>>8 == 0x83 })
	if err != nil {
		conn.Close()
		return bmc.FormatError("Dell ConnectControl read login response", err)
	}
	if len(yb.Payload) < 8 {
		conn.Close()
		return fmt.Errorf("short login response: %d bytes", len(yb.Payload))
	}
	if status := yb.Payload[0]; status != 0 {
		conn.Close()
		return fmt.Errorf("dell control channel authentication rejected with status code %d", status)
	}

	_ = conn.SetDeadline(time.Time{})
	c.ApplianceSessionToken = binary.BigEndian.Uint32(yb.Payload[4:8])
	c.CtrlConn = conn
	log.Printf("[Dell] Control Channel authenticated successfully")
	return nil
}

// ConnectVideo establishes Channel Mode 4 (Video Console) and sends dc authentication.
func (c *Client) ConnectVideo() error {
	c.ConnMu.Lock()
	defer c.ConnMu.Unlock()

	conn, err := c.connectAPCP(c.VPort, channelModeVideo)
	if err != nil {
		return bmc.FormatError("Dell ConnectVideo", err)
	}

	if _, err := conn.Write(videoAuthPacket(c.ClientRandom, c.ApplianceSessionToken)); err != nil {
		conn.Close()
		return bmc.FormatError("Dell ConnectVideo send dc packet", err)
	}

	ac, err := awaitPacket(conn, func(id uint16) bool { return id == pktVideoAuthResp })
	if err != nil {
		conn.Close()
		return bmc.FormatError("Dell ConnectVideo read auth response", err)
	}
	if len(ac.Payload) > 0 && ac.Payload[0] != 0 {
		conn.Close()
		return fmt.Errorf("dell video channel authentication rejected with code %d", ac.Payload[0])
	}

	_ = conn.SetDeadline(time.Time{})
	c.VideoConn = conn
	log.Printf("[Dell] Video Channel authenticated successfully on port %d", c.VPort)
	return nil
}

// EnableVideo sends the fb(true), y(1024, 768), and p() command packets on the Control Channel.
func (c *Client) EnableVideo() error {
	// fb(true); y(1024, 768) with width and height as big-endian uint16s; then p() for 1:1
	// scaling, the sequence the vendor viewer sends at start-up.
	steps := []struct {
		name string
		pkt  []byte
	}{
		{"fb", beefPacket(pktEnableVideo, [8]byte{1, 1})},
		{"y", beefPacket(pktSetResolution, [8]byte{0x04, 0x00, 0x03, 0x00})},
		{"p", beefPacket(pktSetScale1to1, [8]byte{})},
	}
	for _, step := range steps {
		if err := c.writeControl(step.pkt); err != nil {
			return bmc.FormatError("Dell EnableVideo "+step.name, err)
		}
	}

	// Start periodic Control Channel heartbeat (Packet 1024 / q) to prevent 30s BMC inactivity timeout
	c.StartHeartbeat()

	log.Printf("[Dell] Video stream successfully enabled on Control Channel")
	return nil
}

// RequestFrameRefresh sends packet 769 (o, "ScreenRefresh") on the Control Channel to request a
// full screen refresh. This keeps the AST2300 video engine active even when the screen is static.
func (c *Client) RequestFrameRefresh() error {
	return c.writeControl(beefPacket(pktScreenRefresh, [8]byte{}))
}

// writeControl sends one packet on the Control Channel.
func (c *Client) writeControl(pkt []byte) error {
	return c.write(&c.ctrlWriteMu, func() net.Conn { return c.CtrlConn }, "control", pkt)
}

// writeVideo sends one packet on the Video Channel.
func (c *Client) writeVideo(pkt []byte) error {
	return c.write(&c.videoWriteMu, func() net.Conn { return c.VideoConn }, "video", pkt)
}

// write snapshots the connection under ConnMu but writes outside it, so a stalled socket delays
// only writers on the same channel (serialised by writeMu), never input on the other channel,
// heartbeats or the readers that also take ConnMu.
func (c *Client) write(writeMu *sync.Mutex, pick func() net.Conn, channel string, pkt []byte) error {
	c.ConnMu.Lock()
	conn := pick()
	c.ConnMu.Unlock()
	if conn == nil {
		return fmt.Errorf("%s channel is not connected", channel)
	}

	writeMu.Lock()
	defer writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(socketWriteTimeout))
	_, err := conn.Write(pkt)
	return err
}
