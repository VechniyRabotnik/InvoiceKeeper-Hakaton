package storage_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/aeza/invoice-keeper/internal/domain"
	"github.com/aeza/invoice-keeper/internal/storage"
)

func newStore(t *testing.T) *storage.Store {
	t.Helper()

	store, err := storage.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func mustDay(t *testing.T, s string) domain.Day {
	t.Helper()
	d, err := domain.ParseDay(s)
	if err != nil {
		t.Fatalf("день %q: %v", s, err)
	}
	return d
}

// ── Пользователи ──────────────────────────────────────────────────────────

// Пользователь сохраняется и читается по логину и id.
func TestCreateAndReadUser(t *testing.T) {
	store := newStore(t)

	id, err := store.CreateUser("ivan", "hash", string(domain.RoleUser))
	if err != nil {
		t.Fatalf("создание: %v", err)
	}
	if id == 0 {
		t.Fatal("пользователю не выдан id")
	}

	byLogin, err := store.UserByLogin("ivan")
	if err != nil {
		t.Fatalf("поиск по логину: %v", err)
	}
	byID, err := store.UserByID(id)
	if err != nil {
		t.Fatalf("поиск по id: %v", err)
	}
	if byLogin.ID != byID.ID || byLogin.Login != "ivan" {
		t.Fatalf("записи не совпадают: %+v / %+v", byLogin, byID)
	}
	if byLogin.Role != domain.RoleUser {
		t.Fatalf("роль %q", byLogin.Role)
	}
}

// Повторный логин отклоняется доменной ошибкой, а не ошибкой драйвера.
func TestDuplicateLogin(t *testing.T) {
	store := newStore(t)

	if _, err := store.CreateUser("ivan", "hash", string(domain.RoleUser)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser("ivan", "hash", string(domain.RoleUser)); !errors.Is(err, domain.ErrDuplicateLogin) {
		t.Fatalf("ожидалась ErrDuplicateLogin, получено %v", err)
	}
}

// Отсутствующий пользователь даёт ErrNotFound.
func TestMissingUser(t *testing.T) {
	store := newStore(t)

	if _, err := store.UserByLogin("нет"); !domain.IsNotFound(err) {
		t.Fatalf("ожидалась ErrNotFound, получено %v", err)
	}
	if _, err := store.UserByID(999); !domain.IsNotFound(err) {
		t.Fatalf("ожидалась ErrNotFound, получено %v", err)
	}
}

// Обновление меняет только переданные поля.
func TestUpdateUser(t *testing.T) {
	store := newStore(t)

	id, _ := store.CreateUser("ivan", "hash", string(domain.RoleUser))

	if err := store.UpdateUser(id, "ivan-new", ""); err != nil {
		t.Fatalf("обновление логина: %v", err)
	}
	updated, _ := store.UserByID(id)
	if updated.Login != "ivan-new" {
		t.Fatalf("логин %q", updated.Login)
	}
	if updated.PasswordHash != "hash" {
		t.Error("хеш пароля изменился без запроса")
	}
	if updated.Role != domain.RoleUser {
		t.Error("роль изменилась без запроса")
	}

	if err := store.UpdateUser(id, "", "новый-хеш"); err != nil {
		t.Fatalf("обновление пароля: %v", err)
	}
	updated, _ = store.UserByID(id)
	if updated.PasswordHash != "новый-хеш" {
		t.Error("хеш пароля не обновлён")
	}
	if updated.Login != "ivan-new" {
		t.Error("логин затёрт при смене пароля")
	}
}

// Занятый логин при обновлении даёт доменную ошибку.
func TestUpdateUserToTakenLogin(t *testing.T) {
	store := newStore(t)

	_, _ = store.CreateUser("ivan", "h", string(domain.RoleUser))
	id, _ := store.CreateUser("petr", "h", string(domain.RoleUser))

	if err := store.UpdateUser(id, "ivan", ""); !errors.Is(err, domain.ErrDuplicateLogin) {
		t.Fatalf("ожидалась ErrDuplicateLogin, получено %v", err)
	}
}

// Осиротевшие администраторы понижаются до обычных пользователей.
func TestDemoteAdminsExcept(t *testing.T) {
	store := newStore(t)

	first, _ := store.CreateUser("admin", "h", string(domain.RoleAdmin))
	second, _ := store.CreateUser("old-admin", "h", string(domain.RoleAdmin))

	if err := store.DemoteAdminsExcept("admin"); err != nil {
		t.Fatalf("понижение: %v", err)
	}

	kept, _ := store.UserByID(first)
	demoted, _ := store.UserByID(second)

	if kept.Role != domain.RoleAdmin {
		t.Error("текущий администратор потерял роль")
	}
	if demoted.Role != domain.RoleUser {
		t.Errorf("старый администратор остался %s", demoted.Role)
	}
}

// Удаление пользователя убирает его из списка.
func TestDeleteUser(t *testing.T) {
	store := newStore(t)

	id, _ := store.CreateUser("ivan", "h", string(domain.RoleUser))
	if err := store.DeleteUser(id); err != nil {
		t.Fatalf("удаление: %v", err)
	}
	if _, err := store.UserByID(id); !domain.IsNotFound(err) {
		t.Fatalf("пользователь остался: %v", err)
	}
}

// ── Платежи и периоды ─────────────────────────────────────────────────────

func newPayment(t *testing.T, store *storage.Store, name, due string) *domain.RecurringPayment {
	t.Helper()

	p := &domain.RecurringPayment{
		Name:     name,
		Amount:   domain.ParseAmount(45000),
		Currency: domain.RUB,
		Period:   domain.Monthly,
		FirstDue: mustDay(t, due),
	}
	if err := store.CreatePayment(p); err != nil {
		t.Fatalf("создание платежа: %v", err)
	}
	if p.CreatedAt.IsZero() {
		t.Fatal("created_at не заполнен после создания")
	}
	return p
}

// Периоды синхронизируются идемпотентно: повтор не создаёт дублей.
func TestSyncOccurrencesIsIdempotent(t *testing.T) {
	store := newStore(t)
	p := newPayment(t, store, "Аренда", "2026-11-01")
	today := mustDay(t, "2026-10-24")

	if err := store.SyncOccurrences(p, today, 12); err != nil {
		t.Fatalf("синхронизация: %v", err)
	}
	first, err := store.ListOccurrences(p.ID)
	if err != nil {
		t.Fatalf("список: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("периоды не созданы")
	}

	if err := store.SyncOccurrences(p, today, 12); err != nil {
		t.Fatalf("повторная синхронизация: %v", err)
	}
	second, _ := store.ListOccurrences(p.ID)
	if len(second) != len(first) {
		t.Fatalf("после повтора %d периодов вместо %d", len(second), len(first))
	}
}

// Ряд периодов сохраняет якорь дня: 31-е число не съезжает.
func TestOccurrenceDayAnchor(t *testing.T) {
	store := newStore(t)
	p := newPayment(t, store, "Аренда", "2026-01-31")
	today := mustDay(t, "2026-01-01")

	if err := store.SyncOccurrences(p, today, 4); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListOccurrences(p.ID)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"2026-01-31", "2026-02-28", "2026-03-31", "2026-04-30"}
	if len(list) < len(want) {
		t.Fatalf("периодов %d, ожидалось не меньше %d", len(list), len(want))
	}
	for i, w := range want {
		if list[i].DueDate.String() != w {
			t.Errorf("период %d: %s, ожидался %s", i, list[i].DueDate, w)
		}
	}
}

// Прошедшие периоды создаются: платёж, добавленный задним числом, сразу
// становится просроченным.
func TestPastOccurrencesAreCreated(t *testing.T) {
	store := newStore(t)
	p := newPayment(t, store, "Аренда", "2026-05-01")
	today := mustDay(t, "2026-10-24")

	if err := store.SyncOccurrences(p, today, 1); err != nil {
		t.Fatal(err)
	}
	list, _ := store.ListOccurrences(p.ID)

	if len(list) < 6 {
		t.Fatalf("создано %d периодов, ожидалось не меньше 6", len(list))
	}
	if list[0].DueDate.String() != "2026-05-01" {
		t.Errorf("первый период %s", list[0].DueDate)
	}
}

// Отметка оплаты ставится и снимается, дата сохраняется как есть.
func TestSetPaid(t *testing.T) {
	store := newStore(t)
	p := newPayment(t, store, "Аренда", "2026-11-01")
	today := mustDay(t, "2026-10-24")

	if err := store.SyncOccurrences(p, today, 1); err != nil {
		t.Fatal(err)
	}
	list, _ := store.ListOccurrences(p.ID)
	target := list[0]

	paid := mustDay(t, "2026-11-03")
	if err := store.SetPaid(target.ID, &paid); err != nil {
		t.Fatalf("оплата: %v", err)
	}
	after, _ := store.OccurrenceByID(target.ID)
	if after.PaidAt == nil || after.PaidAt.String() != "2026-11-03" {
		t.Fatalf("paid_at = %v", after.PaidAt)
	}

	if err := store.SetPaid(target.ID, nil); err != nil {
		t.Fatalf("снятие: %v", err)
	}
	after, _ = store.OccurrenceByID(target.ID)
	if after.PaidAt != nil {
		t.Fatalf("отметка не снята: %v", after.PaidAt)
	}
}

// Отметка оплаты переживает досинхронизацию периодов.
func TestPaidSurvivesSync(t *testing.T) {
	store := newStore(t)
	p := newPayment(t, store, "Аренда", "2026-11-01")
	today := mustDay(t, "2026-10-24")

	if err := store.SyncOccurrences(p, today, 1); err != nil {
		t.Fatal(err)
	}
	list, _ := store.ListOccurrences(p.ID)
	paid := mustDay(t, "2026-11-02")
	if err := store.SetPaid(list[0].ID, &paid); err != nil {
		t.Fatal(err)
	}

	if err := store.SyncOccurrences(p, today, 12); err != nil {
		t.Fatal(err)
	}
	after, _ := store.OccurrenceByID(list[0].ID)
	if after.PaidAt == nil || after.PaidAt.String() != "2026-11-02" {
		t.Fatalf("оплата потеряна при синхронизации: %v", after.PaidAt)
	}
}

// Напоминание занимается атомарно: второй вызов возвращает false.
func TestMarkReminderSentIsAtomic(t *testing.T) {
	store := newStore(t)
	p := newPayment(t, store, "Аренда", "2026-11-01")
	today := mustDay(t, "2026-10-24")

	if err := store.SyncOccurrences(p, today, 1); err != nil {
		t.Fatal(err)
	}
	list, _ := store.ListOccurrences(p.ID)
	target := list[0]

	claimed, err := store.MarkReminderSent(target.ID)
	if err != nil || !claimed {
		t.Fatalf("первое занятие: claimed=%v err=%v", claimed, err)
	}
	claimed, err = store.MarkReminderSent(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("напоминание занято дважды")
	}
}

// ClearReminder возвращает период в очередь после ошибки отправки.
func TestClearReminder(t *testing.T) {
	store := newStore(t)
	p := newPayment(t, store, "Аренда", "2026-11-01")
	today := mustDay(t, "2026-10-24")

	if err := store.SyncOccurrences(p, today, 1); err != nil {
		t.Fatal(err)
	}
	list, _ := store.ListOccurrences(p.ID)
	target := list[0]

	if claimed, _ := store.MarkReminderSent(target.ID); !claimed {
		t.Fatal("напоминание не занято")
	}
	if err := store.ClearReminder(target.ID); err != nil {
		t.Fatalf("сброс: %v", err)
	}
	if claimed, _ := store.MarkReminderSent(target.ID); !claimed {
		t.Fatal("после сброса напоминание недоступно")
	}
}

// Деактивация платежа сохраняет его историю: периоды не удаляются.
func TestDeactivateKeepsHistory(t *testing.T) {
	store := newStore(t)
	p := newPayment(t, store, "Аренда", "2026-11-01")
	today := mustDay(t, "2026-10-24")

	if err := store.SyncOccurrences(p, today, 3); err != nil {
		t.Fatal(err)
	}
	list, _ := store.ListOccurrences(p.ID)

	if err := store.DeactivatePayment(p.ID); err != nil {
		t.Fatalf("деактивация: %v", err)
	}

	active, _ := store.ListPayments(true)
	for _, x := range active {
		if x.ID == p.ID {
			t.Fatal("деактивированный платёж попал в активные")
		}
	}

	history, err := store.ListOccurrences(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != len(list) {
		t.Fatalf("история сократилась: %d вместо %d", len(history), len(list))
	}
}

// Остановленный платёж не получает новых периодов.
func TestInactivePaymentStopsGrowing(t *testing.T) {
	store := newStore(t)
	p := newPayment(t, store, "Аренда", "2026-11-01")
	today := mustDay(t, "2026-10-24")

	if err := store.SyncOccurrences(p, today, 2); err != nil {
		t.Fatal(err)
	}
	if err := store.DeactivatePayment(p.ID); err != nil {
		t.Fatal(err)
	}

	stored, err := store.PaymentByID(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Active {
		t.Fatal("платёж остался активным")
	}
	if err := store.SyncOccurrences(stored, today, 12); err != nil {
		t.Fatal(err)
	}
	history, _ := store.ListOccurrences(p.ID)
	if len(history) > 3 {
		t.Fatalf("остановленный платёж получил новые периоды: %d", len(history))
	}
}

// Удаление платежа каскадно убирает его периоды (проверка PRAGMA foreign_keys).
func TestDeletePaymentCascades(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cascade.db")
	store, err := storage.New(path)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	p := newPayment(t, store, "Аренда", "2026-11-01")
	today := mustDay(t, "2026-10-24")

	if err := store.SyncOccurrences(p, today, 2); err != nil {
		t.Fatal(err)
	}
	list, _ := store.ListOccurrences(p.ID)
	if len(list) == 0 {
		t.Fatal("периоды не созданы")
	}

	// Каскад проверяем отдельным соединением: внешний ключ должен быть включён
	// на уровне схемы, а не только в текущей сессии.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("второе соединение: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DELETE FROM recurring_payments WHERE id = ?`, p.ID); err != nil {
		t.Fatalf("удаление: %v", err)
	}

	left, err := store.ListOccurrences(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("осталось %d периодов удалённого платежа", len(left))
	}
}

// ── Отозванные токены ─────────────────────────────────────────────────────

// Отзыв сессии фиксируется и очищается после истечения срока.
func TestRevokedTokens(t *testing.T) {
	store := newStore(t)

	expires := time.Now().Add(time.Hour)
	if err := store.RevokeToken("jti-1", expires); err != nil {
		t.Fatalf("отзыв: %v", err)
	}

	revoked, err := store.IsTokenRevoked("jti-1")
	if err != nil || !revoked {
		t.Fatalf("revoked=%v err=%v", revoked, err)
	}

	other, err := store.IsTokenRevoked("jti-2")
	if err != nil || other {
		t.Fatalf("чужой токен помечен отозванным: %v %v", other, err)
	}

	// Запись с истёкшим сроком удаляется очисткой.
	if err := store.RevokeToken("jti-old", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.PurgeExpiredTokens(time.Now()); err != nil {
		t.Fatalf("очистка: %v", err)
	}
	if revoked, _ := store.IsTokenRevoked("jti-old"); revoked {
		t.Fatal("истёкшая запись не удалена")
	}
}

// HasPayments отличает пустую базу от наполненной: на этом построен seed.
func TestHasPayments(t *testing.T) {
	store := newStore(t)

	has, err := store.HasPayments()
	if err != nil || has {
		t.Fatalf("пустая база: has=%v err=%v", has, err)
	}

	newPayment(t, store, "Аренда", "2026-11-01")

	has, err = store.HasPayments()
	if err != nil || !has {
		t.Fatalf("после создания: has=%v err=%v", has, err)
	}
}

// Каталог базы создаётся сам: data/ не хранится в репозитории, и чистый
// клон должен запускаться без ручной подготовки.
func TestNewCreatesMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "test.db")

	store, err := storage.New(path)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	defer store.Close()

	if _, err := store.HasPayments(); err != nil {
		t.Fatalf("база не открылась: %v", err)
	}
}
