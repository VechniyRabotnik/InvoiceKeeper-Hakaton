package ratelimit

import (
	"testing"
	"time"
)

// clock — управляемое время, чтобы проверять окно без реальных задержек.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newLimiter(limit int, window time.Duration) (*Limiter, *clock) {
	c := &clock{t: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	l := New(limit, window)
	l.now = c.now
	return l, c
}

// До исчерпания лимита попытки разрешены.
func TestAllowsUntilLimit(t *testing.T) {
	l, _ := newLimiter(3, time.Minute)

	for i := 0; i < 3; i++ {
		if !l.Allowed("1.2.3.4") {
			t.Fatalf("попытка %d отклонена раньше времени", i+1)
		}
		l.Fail("1.2.3.4")
	}
	if l.Allowed("1.2.3.4") {
		t.Fatal("после исчерпания лимита попытка разрешена")
	}
}

// Лимит считается отдельно по каждому адресу.
func TestLimitsArePerKey(t *testing.T) {
	l, _ := newLimiter(1, time.Minute)

	l.Fail("1.1.1.1")
	if l.Allowed("1.1.1.1") {
		t.Fatal("адрес с исчерпанным лимитом допущен")
	}
	if !l.Allowed("2.2.2.2") {
		t.Fatal("другой адрес заблокирован вместе с первым")
	}
}

// После окна попытки снова разрешены.
func TestWindowExpires(t *testing.T) {
	l, c := newLimiter(1, time.Minute)

	l.Fail("1.1.1.1")
	if l.Allowed("1.1.1.1") {
		t.Fatal("внутри окна попытка разрешена")
	}

	c.advance(time.Minute + time.Second)
	if !l.Allowed("1.1.1.1") {
		t.Fatal("после окна попытка не разрешена")
	}
}

// Успешный вход сбрасывает счётчик.
func TestResetClearsFailures(t *testing.T) {
	l, _ := newLimiter(1, time.Minute)

	l.Fail("1.1.1.1")
	l.Reset("1.1.1.1")
	if !l.Allowed("1.1.1.1") {
		t.Fatal("после сброса попытка не разрешена")
	}
}

// RetryAfter показывает, сколько ждать, и не уходит в минус.
func TestRetryAfter(t *testing.T) {
	l, c := newLimiter(1, time.Minute)

	if d := l.RetryAfter("1.1.1.1"); d != 0 {
		t.Errorf("для неизвестного адреса ожидался 0, получено %s", d)
	}

	l.Fail("1.1.1.1")
	if d := l.RetryAfter("1.1.1.1"); d <= 0 || d > time.Minute {
		t.Errorf("некорректная задержка: %s", d)
	}

	c.advance(2 * time.Minute)
	if d := l.RetryAfter("1.1.1.1"); d != 0 {
		t.Errorf("после окна ожидался 0, получено %s", d)
	}
}

// Sweep освобождает память от истекших записей.
func TestSweepRemovesExpired(t *testing.T) {
	l, c := newLimiter(5, time.Minute)

	l.Fail("1.1.1.1")
	l.Fail("2.2.2.2")
	c.advance(time.Minute + time.Second)
	l.Sweep()

	l.mu.Lock()
	size := len(l.entries)
	l.mu.Unlock()

	if size != 0 {
		t.Fatalf("после очистки осталось %d записей", size)
	}
}

// Ограничитель безопасен при одновременных запросах.
func TestConcurrentAccess(t *testing.T) {
	l, _ := newLimiter(100, time.Minute)

	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				l.Allowed("1.1.1.1")
				l.Fail("1.1.1.1")
				l.RetryAfter("1.1.1.1")
			}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}
