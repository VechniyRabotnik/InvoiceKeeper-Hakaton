// Package session — единый формат сессии для API и веб-панели.
//
// Панель и API используют один и тот же JWT, поэтому claims и проверка подписи
// живут в одном месте: изменить формат токена можно только здесь, и оба
// интерфейса останутся совместимы.
package session

import (
	"errors"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/domain"
)

// Claims — содержимое токена. ID (jti) нужен, чтобы отозвать конкретную сессию
// при logout, не дожидаясь истечения срока.
type Claims struct {
	UserID int64  `json:"uid"`
	Login  string `json:"login"`
	Role   string `json:"role"`
	jwt.RegisteredClaims
}

func (c *Claims) IsAdmin() bool { return c.Role == string(domain.RoleAdmin) }

// Service выпускает и проверяет токены.
type Service struct {
	secret []byte
	ttl    time.Duration
}

func New(secret []byte, ttl time.Duration) *Service {
	return &Service{secret: secret, ttl: ttl}
}

// TTL — время жизни сессии: нужно панели для срока cookie.
func (s *Service) TTL() time.Duration { return s.ttl }

func (s *Service) Issue(u *domain.User) (string, error) {
	claims := Claims{
		UserID: u.ID,
		Login:  u.Login,
		Role:   string(u.Role),
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        domain.NewID(),
			Subject:   strconv.FormatInt(u.ID, 10),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(s.ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
}

// Verify проверяет подпись, срок и алгоритм. Явное ограничение алгоритма
// исключает подмену на "none" и подобные атаки.
func (s *Service) Verify(raw string) (*Claims, error) {
	var claims Claims
	_, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("неожиданный алгоритм подписи")
		}
		return s.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return nil, err
	}
	return &claims, nil
}
