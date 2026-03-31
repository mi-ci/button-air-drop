package api

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

func (s *Server) broadcastLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			state, err := s.game.Snapshot()
			if err != nil {
				log.Printf("broadcast snapshot failed: %v", err)
				continue
			}
			s.wsHub.broadcastState(state)
		}
	}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgradeWebSocket(w, r)
	if err != nil {
		http.Error(w, "websocket upgrade failed", http.StatusBadRequest)
		return
	}

	s.wsHub.add(conn)

	state, err := s.game.Snapshot()
	if err == nil {
		_ = conn.WriteJSON(map[string]any{
			"type":  "state",
			"state": state,
		})
	}
}

type wsHub struct {
	mu      sync.Mutex
	clients map[*wsConn]struct{}
}

func newWSHub() *wsHub {
	return &wsHub{clients: map[*wsConn]struct{}{}}
}

func (h *wsHub) add(conn *wsConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[conn] = struct{}{}
}

func (h *wsHub) broadcastState(state any) {
	payload, err := json.Marshal(map[string]any{
		"type":  "state",
		"state": state,
	})
	if err != nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for conn := range h.clients {
		if err := conn.WriteText(payload); err != nil {
			_ = conn.Close()
			delete(h.clients, conn)
		}
	}
}

func (h *wsHub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for conn := range h.clients {
		_ = conn.Close()
		delete(h.clients, conn)
	}
}

type wsConn struct {
	net.Conn
	br *bufio.ReadWriter
	mu sync.Mutex
}

func (c *wsConn) WriteJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.WriteText(data)
}

func (c *wsConn) WriteText(payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	header := []byte{0x81}
	payloadLen := len(payload)
	switch {
	case payloadLen <= 125:
		header = append(header, byte(payloadLen))
	case payloadLen <= 65535:
		header = append(header, 126, byte(payloadLen>>8), byte(payloadLen))
	default:
		return errors.New("payload too large")
	}

	if _, err := c.br.Write(header); err != nil {
		return err
	}
	if _, err := c.br.Write(payload); err != nil {
		return err
	}
	return c.br.Flush()
}

func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !headerContainsToken(r.Header, "Connection", "Upgrade") || !headerContainsToken(r.Header, "Upgrade", "websocket") {
		return nil, errors.New("missing websocket upgrade headers")
	}

	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		return nil, errors.New("missing websocket key")
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("hijacking not supported")
	}

	conn, br, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}

	accept := websocketAccept(key)
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n" +
		"\r\n"
	if _, err := br.WriteString(response); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := br.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return &wsConn{Conn: conn, br: br}, nil
}

func websocketAccept(key string) string {
	hash := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(hash[:])
}

func headerContainsToken(header http.Header, key, token string) bool {
	for _, value := range header.Values(key) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}
