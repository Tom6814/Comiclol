// Package session 维护各来源的登录会话（内存态）。
//
// 它本身不感知具体来源，只是 sourceID -> domain.Session 的带锁映射，
// 由各来源插件在登录成功后写入。下载引擎、HTTP 层、同步服务都从这里
// 取当前会话，统一了「当前登录态」这一概念的来源。
package session

import (
	"sync"

	"tsukimi/internal/domain"
)

type Manager struct {
	mu       sync.RWMutex
	sessions map[string]domain.Session
}

func New() *Manager {
	return &Manager{sessions: map[string]domain.Session{}}
}

func (m *Manager) Set(s domain.Session) {
	m.mu.Lock()
	m.sessions[s.SourceID] = s
	m.mu.Unlock()
}

func (m *Manager) Get(sourceID string) (domain.Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := sessionsAlive(m.sessions[sourceID])
	return s, ok
}

func (m *Manager) Clear(sourceID string) {
	m.mu.Lock()
	delete(m.sessions, sourceID)
	m.mu.Unlock()
}

func (m *Manager) All() []domain.Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]domain.Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

// sessionsAlive 返回会话；过期则视为不存在。
func sessionsAlive(s domain.Session) (domain.Session, bool) {
	if s.SourceID == "" {
		return s, false
	}
	if !s.ValidUntil.IsZero() && s.ValidUntil.Before(timeNow()) {
		return s, false
	}
	return s, true
}
