// Package ratelimit — ограничение частоты попыток входа.
//
// Защита нужна именно на входе: перебор пароля — единственная операция,
// которую может запускать неавторизованный клиент. Счётчик живёт в памяти и
// привязан к IP; для одного экземпляра приложения этого достаточно, а при
// горизонтальном масштабировании заменяется на Redis без изменения API.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter считает неудачные попытки в скользящем окне.
type Limiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	now     func() time.Time
	entries map[string]*entry
}

type entry struct {
	failures int
	resetAt  time.Time
}

// New создаёт ограничитель: не более limit неудачных попыток за window.
func New(limit int, window time.Duration) *Limiter {
	return &Limiter{
		limit:   limit,
		window:  window,
		now:     time.Now,
		entries: make(map[string]*entry),
	}
}

// Allowed сообщает, можно ли пробовать вход с этого адреса.
func (l *Limiter) Allowed(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[key]
	if !ok || l.now().After(e.resetAt) {
		return true
	}
	return e.failures < l.limit
}

func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	e, ok := l.entries[key]
	if !ok || now.After(e.resetAt) {
		l.entries[key] = &entry{failures: 1, resetAt: now.Add(l.window)}
		return
	}
	e.failures++
}

// Reset сбрасывает счётчик после успешного входа: пользователь, вспомнивший
// пароль, не должен оставаться заблокированным.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// RetryAfter — сколько ждать до новой попытки.
func (l *Limiter) RetryAfter(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[key]
	if !ok {
		return 0
	}
	if d := e.resetAt.Sub(l.now()); d > 0 {
		return d
	}
	return 0
}

// Sweep удаляет истекшие записи, чтобы карта не росла от разовых адресов.
func (l *Limiter) Sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	for key, e := range l.entries {
		if now.After(e.resetAt) {
			delete(l.entries, key)
		}
	}
}
