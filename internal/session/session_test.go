package session_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aeza/invoice-keeper/internal/domain"
	"github.com/aeza/invoice-keeper/internal/session"
)

func testUser() *domain.User {
	return &domain.User{ID: 7, Login: "admin", Role: domain.RoleAdmin}
}

// Выпущенный токен проверяется и сохраняет данные пользователя.
func TestIssueAndVerify(t *testing.T) {
	svc := session.New([]byte("secret"), time.Hour)

	token, err := svc.Issue(testUser())
	if err != nil {
		t.Fatalf("выпуск: %v", err)
	}

	claims, err := svc.Verify(token)
	if err != nil {
		t.Fatalf("проверка: %v", err)
	}
	if claims.UserID != 7 || claims.Login != "admin" || !claims.IsAdmin() {
		t.Fatalf("некорректные claims: %+v", claims)
	}
	if claims.ID == "" {
		t.Error("у токена нет идентификатора сессии (jti) — logout не сможет его отозвать")
	}
	if claims.ExpiresAt == nil || time.Until(claims.ExpiresAt.Time) > time.Hour {
		t.Error("срок жизни токена не соответствует настройке")
	}
}

// Каждый выпуск даёт уникальный jti: отзыв одной сессии не рвёт другую.
func TestEachIssueHasUniqueID(t *testing.T) {
	svc := session.New([]byte("secret"), time.Hour)

	first, _ := svc.Issue(testUser())
	second, _ := svc.Issue(testUser())

	a, _ := svc.Verify(first)
	b, _ := svc.Verify(second)

	if a.ID == b.ID {
		t.Fatal("две сессии получили одинаковый jti")
	}
}

// Токен, подписанный другим секретом, не принимается.
func TestWrongSecretRejected(t *testing.T) {
	issuer := session.New([]byte("secret"), time.Hour)
	other := session.New([]byte("другой-секрет"), time.Hour)

	token, _ := issuer.Issue(testUser())
	if _, err := other.Verify(token); err == nil {
		t.Fatal("токен с чужой подписью принят")
	}
}

// Истёкший токен не принимается.
func TestExpiredTokenRejected(t *testing.T) {
	svc := session.New([]byte("secret"), -time.Minute)

	token, err := svc.Issue(testUser())
	if err != nil {
		t.Fatalf("выпуск: %v", err)
	}
	if _, err := svc.Verify(token); err == nil {
		t.Fatal("истёкший токен принят")
	}
}

// Мусор и подделки не проходят проверку.
func TestMalformedTokenRejected(t *testing.T) {
	svc := session.New([]byte("secret"), time.Hour)

	for _, raw := range []string{
		"",
		"просто-строка",
		"a.b.c",
		"eyJhbGciOiJub25lIn0.eyJ1aWQiOjF9.", // alg=none
	} {
		if _, err := svc.Verify(raw); err == nil {
			t.Errorf("подделка %q принята", raw)
		}
	}
}

// Роль обычного пользователя не даёт административных прав.
func TestIsAdminForRegularUser(t *testing.T) {
	svc := session.New([]byte("secret"), time.Hour)

	token, _ := svc.Issue(&domain.User{ID: 2, Login: "demo", Role: domain.RoleUser})
	claims, err := svc.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.IsAdmin() {
		t.Fatal("обычный пользователь получил права администратора")
	}
}

// TTL отдаётся наружу: панель ставит по нему срок жизни cookie.
func TestTTL(t *testing.T) {
	svc := session.New([]byte("secret"), 90*time.Minute)
	if svc.TTL() != 90*time.Minute {
		t.Fatalf("TTL %s", svc.TTL())
	}
}

// Подпись токена не содержит секрет в открытом виде.
func TestTokenDoesNotLeakSecret(t *testing.T) {
	const secret = "супер-секретная-строка"
	svc := session.New([]byte(secret), time.Hour)

	token, _ := svc.Issue(testUser())
	if strings.Contains(token, secret) {
		t.Fatal("секрет виден в токене")
	}
}
