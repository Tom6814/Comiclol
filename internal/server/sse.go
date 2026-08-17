package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"tsukimi/internal/plugin"
)

// Broker 把 EventBus 上的事件以 SSE（Server-Sent Events）形式推给前端。
//
// 每个连接会启动一个订阅者；事件被复制到该订阅者的 buffered channel。
// 这样的事件流让下载进度、同步状态等实时变化无需轮询就能反映到 UI。
type Broker struct {
	mu      sync.RWMutex
	clients map[chan event]struct{}

	unsub func()
}

type event struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

func NewBroker(bus *plugin.EventBus, logger *plugin.Logger) *Broker {
	b := &Broker{clients: map[chan event]struct{}{}}
	bus.On("*", func(ctx context.Context, ev plugin.Event) error {
		b.broadcast(event{Type: ev.Type, Payload: ev.Payload})
		return nil
	})
	return b
}

func (b *Broker) broadcast(e event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.clients {
		select {
		case ch <- e:
		default:
			// 慢客户端：丢弃，避免拖累其他人
		}
	}
}

func (b *Broker) subscribe() chan event {
	ch := make(chan event, 64)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Broker) unsubscribe(ch chan event) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
	close(ch)
}

func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := b.subscribe()
	defer b.unsubscribe(ch)

	ctx := r.Context()
	ping := timeTick()
	defer ping.Stop()

	// 先发一次 hello，方便前端确认连接建立
	fmt.Fprintf(w, "event: hello\ndata: {\"ok\":true}\n\n")
	flusher.Flush()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case ev := <-ch:
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
			flusher.Flush()
		}
	}
}
