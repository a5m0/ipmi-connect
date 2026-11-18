package web

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/coder/websocket"
)

// maxClientMessage bounds browser-to-server messages: input events are tiny, a paste to type
// is up to maxTypeRunes characters.
const maxClientMessage = 6*maxTypeRunes + 1024 // JSON escapes a character as up to 6 bytes

// statusSessionEnded tells the page that the BMC session it was showing has ended (disconnect),
// so it goes back to the connection manager instead of reconnecting the socket.
const statusSessionEnded websocket.StatusCode = 4000

// handleWS streams video to one browser and forwards its input to the attached console. The
// guard has already checked the Host, the Origin and the single-use ticket; Accept checks the
// Origin again against the Host.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	b := s.bound()
	if b == nil {
		http.Error(w, "no BMC is connected", http.StatusConflict)
		return
	}
	if s.clientCount() >= maxClients {
		http.Error(w, "too many browser connections", http.StatusServiceUnavailable)
		return
	}
	// Registered first so they run last: after the input release below.
	s.connWG.Add(1)
	defer s.connWG.Done()
	b.wg.Add(1)
	defer b.wg.Done()

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Printf("[WS] Rejected connection: %v", err)
		return
	}
	conn.SetReadLimit(maxClientMessage)

	// Not r.Context(): the request is hijacked, so its context does not track the socket. The
	// binding's context ends on disconnect, logout and shutdown.
	ctx, cancel := context.WithCancel(b.ctx)
	defer cancel()
	defer func() {
		if b.ctx.Err() != nil {
			_ = conn.Close(statusSessionEnded, "session ended")
			return
		}
		_ = conn.CloseNow()
	}()

	client := newWSClient(cancel)
	go client.writeLoop(ctx, cancel, conn)

	s.StateMu.RLock()
	width, height := s.State.ScreenWidth, s.State.ScreenHeight
	s.StateMu.RUnlock()
	resize, _ := json.Marshal(map[string]any{"type": "resize", "width": width, "height": height})
	client.out <- outMessage{typ: websocket.MessageText, data: resize}

	s.attachClient(client)
	defer s.removeClient(client)

	input := newInputState(b.console)
	input.typing = &s.typing
	defer input.releaseAll() // a closed tab must not leave keys or buttons held on the host
	// Ask the BMC for a complete frame even when a picture exists: reloading the page is how
	// a user recovers one damaged by a lost or corrupt frame.
	if err := b.console.RequestFrameRefresh(); err != nil {
		log.Printf("[WS] Refresh for new client: %v", err)
	}

	var typing context.CancelFunc // the paste this browser is typing, if any
	defer func() {
		if typing != nil {
			typing()
		}
	}()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		s.touch()
		if typ == websocket.MessageText {
			s.handleTyping(ctx, b, client, input, data, &typing)
			input.handle(data)
		}
	}
}
