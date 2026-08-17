// Package plugin implements a cordis-inspired plugin system.
//
// In cordis, a Context is passed to every plugin; the plugin registers the
// services it provides (or consumes services others registered) and binds to
// the bus. We mirror that here: a Plugin receives a *Context in Apply(), and
// can publish/subscribe to events, register sources/sinks/hooks, and read
// shared services. This keeps the whole app "everything-is-a-plugin".
package plugin

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// Manifest describes a plugin.
type Manifest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Author      string `json:"author"`
}

// Plugin is the unit of extensibility. Apply wires the plugin into the host.
// Returning an error aborts startup.
type Plugin interface {
	Manifest() Manifest
	Apply(ctx *Context) error
}

// Logger is a tiny leveled logger with plugin-scoped prefixes.
type Logger struct {
	std *log.Logger
	mu  sync.Mutex
}

func NewLogger() *Logger {
	return &Logger{std: log.New(os.Stderr, "", log.LstdFlags)}
}

func (l *Logger) logf(level, scope, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.std.Printf("%s [%s] %s\n", level, scope, fmt.Sprintf(format, args...))
}

func (l *Logger) Infof(scope, format string, args ...any)  { l.logf("INFO ", scope, format, args...) }
func (l *Logger) Warnf(scope, format string, args ...any)  { l.logf("WARN ", scope, format, args...) }
func (l *Logger) Errorf(scope, format string, args ...any) { l.logf("ERROR", scope, format, args...) }
func (l *Logger) Debugf(scope, format string, args ...any) { l.logf("DEBUG", scope, format, args...) }

// Event is a message on the bus.
type Event struct {
	Type    string         // e.g. "download.image.after"
	Time    time.Time      // when it was published
	Payload map[string]any // arbitrary typed payload
}

// Handler processes an event. Errors are logged but do not stop other handlers.
type Handler func(ctx context.Context, ev Event) error

// EventBus is a synchronous in-process pub/sub.
type EventBus struct {
	mu       sync.RWMutex
	handlers map[string][]Handler
	logger   *Logger
}

func NewBus(logger *Logger) *EventBus {
	return &EventBus{handlers: map[string][]Handler{}, logger: logger}
}

// On subscribes to a topic. Use "*" to receive all events.
func (b *EventBus) On(topic string, h Handler) {
	b.mu.Lock()
	b.handlers[topic] = append(b.handlers[topic], h)
	b.mu.Unlock()
}

// Publish dispatches an event to matching handlers plus wildcard handlers.
func (b *EventBus) Publish(ctx context.Context, ev Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	b.mu.RLock()
	subs := append([]Handler{}, b.handlers[ev.Type]...)
	subs = append(subs, b.handlers["*"]...)
	b.mu.RUnlock()
	for _, h := range subs {
		if err := h(ctx, ev); err != nil {
			b.logger.Errorf("bus", "handler for %s failed: %v", ev.Type, err)
		}
	}
}

// Helper to publish with a payload map built inline.
func (b *EventBus) Emit(ctx context.Context, topic string, kv ...any) {
	payload := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		k := fmt.Sprintf("%v", kv[i])
		payload[k] = kv[i+1]
	}
	b.Publish(ctx, Event{Type: topic, Payload: payload})
}

// Context is the host environment handed to every plugin.
// Services are registered here and consumed by name, mirroring cordis.
type Context struct {
	Logger *Logger
	Bus    *EventBus

	mu       sync.Mutex
	services map[string]any
	plugins  []Plugin
	disposed bool
}

func NewContext(logger *Logger, bus *EventBus) *Context {
	return &Context{Logger: logger, Bus: bus, services: map[string]any{}}
}

// Provide registers a named service. Overwrites prior registration.
func (c *Context) Provide(name string, svc any) {
	c.mu.Lock()
	c.services[name] = svc
	c.mu.Unlock()
}

// Resolve fetches a service and type-asserts into T. ok=false if missing or wrong type.
func Resolve[T any](c *Context, name string) (svc T, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, exists := c.services[name]
	if !exists {
		return svc, false
	}
	svc, ok = v.(T)
	return svc, ok
}

// Services returns a snapshot of registered service names.
func (c *Context) Services() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	names := make([]string, 0, len(c.services))
	for k := range c.services {
		names = append(names, k)
	}
	return names
}

// Register adds a plugin instance to the host (does not Apply).
func (c *Context) Register(p Plugin) {
	c.mu.Lock()
	c.plugins = append(c.plugins, p)
	c.mu.Unlock()
}

// Boot applies every registered plugin in order.
func (c *Context) Boot() error {
	c.mu.Lock()
	plugins := append([]Plugin{}, c.plugins...)
	c.mu.Unlock()
	for _, p := range plugins {
		m := p.Manifest()
		c.Logger.Infof("plugin", "applying %s (%s) %s", m.ID, m.Name, m.Version)
		if err := p.Apply(c); err != nil {
			return fmt.Errorf("plugin %s: %w", m.ID, err)
		}
	}
	return nil
}

// Dispose is a placeholder for graceful shutdown of plugins.
func (c *Context) Dispose() {
	c.mu.Lock()
	c.disposed = true
	c.mu.Unlock()
	c.Bus.Emit(context.Background(), "host.dispose")
}
