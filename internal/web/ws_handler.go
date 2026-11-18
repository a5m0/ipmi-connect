package web

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// maxClientMessage bounds browser-to-server messages: input events are tiny, a paste to type
// is up to maxTypeRunes characters.
const maxClientMessage = 6*maxTypeRunes + 1024 // JSON escapes a character as up to 6 bytes

// closeGrace bounds the last message to a browser whose socket is being closed.
const closeGrace = time.Second

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
	if !b.acquire() {
		http.Error(w, "the BMC session is ending", http.StatusConflict)
		return
	}
	defer b.release()
	s.connWG.Add(1)
	defer s.connWG.Done()

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Printf("[WS] Rejected connection: %v", err)
		return
	}
	conn.SetReadLimit(maxClientMessage)

	defer func() { _ = conn.CloseNow() }()

	// Not r.Context(): the request is hijacked, so its context does not track the socket. ctx
	// ends on disconnect and shutdown (the binding) or logout (client.close). Reads use their
	// own context: the watcher below first tells the page why the socket is ending, then
	// cancels the read, which drops the connection.
	ctx, cancel := context.WithCancel(b.ctx)
	defer cancel()
	readCtx, readCancel := context.WithCancel(context.Background())
	defer readCancel()
	watcherDone := make(chan struct{})
	// Runs after the input release below and before CloseNow: lets the watcher's last message
	// go out (bounded by closeGrace).
	defer func() { cancel(); <-watcherDone }()
	go func() {
		defer close(watcherDone)
		<-ctx.Done()
		// Tell the page why before dropping the connection. (A close frame would also say it,
		// but a close handshake racing the read loop can send two, which browsers reject.)
		reason := "signed out"
		if b.ctx.Err() != nil {
			reason = "session ended"
		}
		msg, _ := json.Marshal(map[string]string{"type": "session-ended", "reason": reason})
		wctx, wcancel := context.WithTimeout(context.Background(), closeGrace)
		_ = conn.Write(wctx, websocket.MessageText, msg)
		wcancel()
		readCancel()
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
		typ, data, err := conn.Read(readCtx)
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
