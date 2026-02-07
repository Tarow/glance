package glance

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

type eventHub struct {
	mu                        sync.Mutex
	clients                   map[chan []byte]struct{}
	lastMonitorEventTimes     map[uint64]time.Time // debounce per widget
	monitorEventDebounceTime  time.Duration
}

func newEventHub() *eventHub {
	return &eventHub{
		clients:                  make(map[chan []byte]struct{}),
		lastMonitorEventTimes:    make(map[uint64]time.Time),
		monitorEventDebounceTime: 5 * time.Second,
	}
}

func (h *eventHub) register() chan []byte {
	ch := make(chan []byte, 8)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *eventHub) unregister(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *eventHub) broadcast(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for ch := range h.clients {
		select {
		case ch <- msg:
		default:
			// drop message for slow client
		}
	}
}

// global hub instance
var globalEventHub *eventHub

// handleEvents serves an SSE stream to the client
func (a *application) handleEvents(w http.ResponseWriter, r *http.Request) {
	if globalEventHub == nil {
		http.Error(w, "events not available", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// security: simple auth via same cookie handling as other endpoints
	if a.handleUnauthorizedResponse(w, r, showUnauthorizedJSON) {
		return
	}

	// set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	msgCh := globalEventHub.register()
	defer globalEventHub.unregister(msgCh)

	ctx := r.Context()

	// send a ping every 30s to keep connection alive
	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	// initial comment to establish stream
	w.Write([]byte(": ok\n\n"))
	flusher.Flush()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-msgCh:
			if !ok {
				return
			}
			w.Write([]byte("data: "))
			w.Write(msg)
			w.Write([]byte("\n\n"))
			flusher.Flush()
		case <-pingTicker.C:
			// send a keep-alive comment
			w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		}
	}
}

// helper to publish JSON event
func publishEvent(eventType string, payload any) {
	if globalEventHub == nil {
		return
	}

	// debounce monitor events per widget to avoid flapping
	if eventType == "monitor:site_changed" {
		if payloadMap, ok := payload.(map[string]any); ok {
			if widgetID, ok := payloadMap["widget_id"].(float64); ok {
				widgetIDUint := uint64(widgetID)
				globalEventHub.mu.Lock()
				if lastTime, exists := globalEventHub.lastMonitorEventTimes[widgetIDUint]; exists {
					if time.Since(lastTime) < globalEventHub.monitorEventDebounceTime {
						globalEventHub.mu.Unlock()
						return // drop event, too soon
					}
				}
				globalEventHub.lastMonitorEventTimes[widgetIDUint] = time.Now()
				globalEventHub.mu.Unlock()
			}
		}
	}

	wrapper := map[string]any{
		"type": eventType,
		"time": time.Now().Unix(),
		"data": payload,
	}

	b, err := json.Marshal(wrapper)
	if err != nil {
		log.Printf("failed to marshal event: %v", err)
		return
	}

	globalEventHub.broadcast(b)
}
