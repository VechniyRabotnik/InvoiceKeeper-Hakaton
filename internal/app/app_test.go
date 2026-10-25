package app_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/app"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/domain"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/storage"
)

// newApp поднимает сервис на временной БД — каждый тест изолирован.
func newApp(t *testing.T) *app.App {
	t.Helper()

	store, err := storage.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	svc, err := app.New(store, "Europe/Moscow")
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	if err := svc.Bootstrap("admin", "admin123"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return svc
}

func createPayment(t *testing.T, svc *app.App, name, due string, amount float64, currency domain.Currency) *domain.RecurringPayment {
	t.Helper()
	p, err := svc.CreatePayment(app.PaymentInput{
		Name: name, Amount: amount, Currency: currency,
		Period: domain.Monthly, DueDate: due,
	})
	if err != nil {
		t.Fatalf("создание платежа %q: %v", name, err)
	}
	return p
}

// Администратор из .env создаётся один раз и не дублируется при рестарте.
func TestBootstrapIsIdempotent(t *testing.T) {
	svc := newApp(t)

	if err := svc.Bootstrap("admin", "admin123"); err != nil {
		t.Fatalf("повторный bootstrap: %v", err)
	}
	users, err := svc.Users()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatalf("ожидался один администратор, получено %d учётных записей", len(users))
	}
	if !users[0].IsAdmin() {
		t.Fatal("учётная запись из .env должна иметь роль admin")
	}
}

// Смена ADMIN_LOGIN не должна оставлять вторую админскую учётную запись.
func TestBootstrapDemotesStaleAdmin(t *testing.T) {
	svc := newApp(t)
	if err := svc.Bootstrap("chief", "chief123"); err != nil {
		t.Fatalf("bootstrap с новым логином: %v", err)
	}

	old, err := svc.Authenticate("admin", "admin123")
	if err != nil {
		t.Fatalf("старый логин должен остаться доступным: %v", err)
	}
	if old.IsAdmin() {
		t.Fatal("после смены ADMIN_LOGIN прежний администратор должен стать обычным пользователем")
	}

	chief, err := svc.Authenticate("chief", "chief123")
	if err != nil {
		t.Fatal(err)
	}
	if !chief.IsAdmin() {
		t.Fatal("администратор из .env должен иметь роль admin")
	}
}

// Пароль администратора берётся из .env при каждом старте: панель его не меняет.
func TestBootstrapSyncsAdminPassword(t *testing.T) {
	svc := newApp(t)
	if err := svc.Bootstrap("admin", "newpassword"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate("admin", "newpassword"); err != nil {
		t.Fatalf("новый пароль из .env не применён: %v", err)
	}
	if _, err := svc.Authenticate("admin", "admin123"); err == nil {
		t.Fatal("старый пароль не должен работать после смены .env")
	}
}

// Неверный логин и неверный пароль дают одинаковую ошибку — нельзя выяснить,
// существует ли учётная запись.
func TestAuthenticateHidesAccountExistence(t *testing.T) {
	svc := newApp(t)

	_, errUnknown := svc.Authenticate("nobody", "secret123")
	_, errWrong := svc.Authenticate("admin", "wrongpass")
	if errUnknown == nil || errWrong == nil {
		t.Fatal("ожидались ошибки аутентификации")
	}
	if errUnknown.Error() != errWrong.Error() {
		t.Fatalf("сообщения различаются: %q и %q", errUnknown, errWrong)
	}
}

// Администратора нельзя ни изменить, ни удалить через сервис (а значит, и
// через панель или API).
func TestAdminAccountIsProtected(t *testing.T) {
	svc := newApp(t)
	admin, err := svc.Authenticate("admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.UpdateUser(admin.ID, "hacked", ""); err == nil {
		t.Fatal("изменение администратора должно быть запрещено")
	}
	if err := svc.DeleteUser(admin.ID); err == nil {
		t.Fatal("удаление администратора должно быть запрещено")
	}

	if _, err := svc.Authenticate("admin", "admin123"); err != nil {
		t.Fatalf("администратор должен остаться работоспособным: %v", err)
	}
}

// Периоды создаются от первого срока вперёд и не дублируются при синхронизации.
func TestSyncOccurrencesIsIdempotent(t *testing.T) {
	svc := newApp(t)
	p := createPayment(t, svc, "Аренда", "2026-11-01", 45000, domain.RUB)

	first, err := svc.Occurrences(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("периоды не созданы")
	}
	if got := first[0].DueDate.String(); got != "2026-11-01" {
		t.Fatalf("первый период: получено %s", got)
	}

	if err := svc.Sync(); err != nil {
		t.Fatal(err)
	}
	second, err := svc.Occurrences(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != len(first) {
		t.Fatalf("синхронизация продублировала периоды: %d → %d", len(first), len(second))
	}
}

// Новый период не стирает прошлые оплаты: отметка живёт в своём периоде.
func TestPaidPeriodSurvivesNewPeriods(t *testing.T) {
	svc := newApp(t)
	p := createPayment(t, svc, "Сервер", "2026-11-05", 120, domain.USD)

	list, err := svc.Occurrences(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	paidID := list[0].ID
	if _, err := svc.SetPaid(paidID, "2026-11-04"); err != nil {
		t.Fatal(err)
	}

	if err := svc.Sync(); err != nil {
		t.Fatal(err)
	}

	after, err := svc.Occurrences(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) < 2 {
		t.Fatal("ожидались минимум два периода")
	}
	if after[0].ID != paidID || after[0].PaidAt == nil {
		t.Fatal("отметка об оплате первого периода потерялась")
	}
	if after[1].PaidAt != nil {
		t.Fatal("следующий период должен быть неоплаченным")
	}
}

// Три операции кейса одним методом: подтвердить, исправить дату, отменить.
func TestSetPaidLifecycle(t *testing.T) {
	svc := newApp(t)
	p := createPayment(t, svc, "Подписка", "2026-11-10", 8.75, domain.EUR)

	list, _ := svc.Occurrences(p.ID)
	id := list[0].ID

	paid, err := svc.SetPaid(id, "2026-11-09")
	if err != nil {
		t.Fatal(err)
	}
	if paid.Status != domain.StatusPaid || paid.PaidAt == nil || paid.PaidAt.String() != "2026-11-09" {
		t.Fatalf("подтверждение оплаты: %+v", paid)
	}

	fixed, err := svc.SetPaid(id, "2026-11-12")
	if err != nil {
		t.Fatal(err)
	}
	if fixed.PaidAt.String() != "2026-11-12" {
		t.Fatalf("исправление даты: получено %s", fixed.PaidAt)
	}
	if fixed.DueDate.String() != "2026-11-10" {
		t.Fatal("срок не должен меняться при исправлении даты оплаты")
	}

	cleared, err := svc.SetPaid(id, "")
	if err != nil {
		t.Fatal(err)
	}
	if cleared.PaidAt != nil {
		t.Fatal("отмена оплаты должна очищать дату")
	}
	if cleared.Status == domain.StatusPaid {
		t.Fatal("после отмены период не может оставаться оплаченным")
	}
}

// Отметка об оплате действует только на конкретный период.
func TestSetPaidIsScopedToOnePeriod(t *testing.T) {
	svc := newApp(t)
	p := createPayment(t, svc, "Аренда", "2026-11-01", 45000, domain.RUB)

	list, _ := svc.Occurrences(p.ID)
	if _, err := svc.SetPaid(list[0].ID, "2026-11-01"); err != nil {
		t.Fatal(err)
	}

	after, _ := svc.Occurrences(p.ID)
	for _, o := range after[1:] {
		if o.PaidAt != nil {
			t.Fatalf("оплата задела чужой период %d", o.ID)
		}
	}
}

// Удалённый платёж не напоминает и не создаёт новых периодов, история остаётся.
func TestStoppedPaymentDoesNotRemind(t *testing.T) {
	svc := newApp(t)
	today := svc.Today()

	p := createPayment(t, svc, "Старый сервер", today.AddDays(1).String(), 50, domain.USD)

	pending, err := svc.PendingReminders()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) == 0 {
		t.Fatal("платёж со сроком завтра должен попасть в напоминания")
	}

	if err := svc.StopPayment(p.ID); err != nil {
		t.Fatal(err)
	}

	pending, err = svc.PendingReminders()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("снятый платёж не должен напоминать, получено %d", len(pending))
	}

	history, err := svc.Occurrences(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 {
		t.Fatal("история периодов должна сохраниться после прекращения платежа")
	}

	active, _ := svc.Payments(true)
	for _, a := range active {
		if a.ID == p.ID {
			t.Fatal("прекращённый платёж не должен попадать в список активных")
		}
	}
}

// Напоминание приходит за 3 дня и не дублируется; оплаченный период молчит.
func TestPendingRemindersWindowAndDedup(t *testing.T) {
	svc := newApp(t)
	today := svc.Today()

	soon := createPayment(t, svc, "Скоро", today.AddDays(3).String(), 100, domain.RUB)
	later := createPayment(t, svc, "Позже", today.AddDays(10).String(), 200, domain.USD)

	pending, err := svc.PendingReminders()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].PaymentID != soon.ID {
		t.Fatalf("ожидалось одно напоминание по ближнему платежу, получено %+v", pending)
	}
	_ = later

	// Занимаем напоминание — повторно оно не выдаётся.
	claimed, err := svc.ClaimReminder(pending[0].ID)
	if err != nil || !claimed {
		t.Fatalf("первое занятие должно вернуть true (err=%v, claimed=%v)", err, claimed)
	}
	again, err := svc.ClaimReminder(pending[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Fatal("повторное занятие того же напоминания должно вернуть false")
	}

	pending, _ = svc.PendingReminders()
	if len(pending) != 0 {
		t.Fatalf("после отправки напоминаний быть не должно, получено %d", len(pending))
	}
}

// Оплаченный период не напоминает, даже если срок уже близко.
func TestPaidPeriodDoesNotRemind(t *testing.T) {
	svc := newApp(t)
	today := svc.Today()

	p := createPayment(t, svc, "Оплачено", today.AddDays(2).String(), 300, domain.EUR)
	list, _ := svc.Occurrences(p.ID)
	if _, err := svc.SetPaid(list[0].ID, today.String()); err != nil {
		t.Fatal(err)
	}

	pending, err := svc.PendingReminders()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("оплаченный период не должен напоминать, получено %d", len(pending))
	}
}

// «Добавили платёж позже чем за 3 дня до срока → напоминание сразу»:
// прошедший срок попадает в выборку немедленно.
func TestOverduePaymentRemindsImmediately(t *testing.T) {
	svc := newApp(t)
	today := svc.Today()

	createPayment(t, svc, "Забытый", today.AddDays(-5).String(), 500, domain.RUB)

	pending, err := svc.PendingReminders()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) == 0 {
		t.Fatal("период с прошедшим сроком должен требовать напоминания сразу")
	}
	if pending[0].Status != domain.StatusOverdue {
		t.Fatalf("статус периода: %s", pending[0].Status)
	}
}

// Итоги считаются раздельно по валютам и не смешиваются.
func TestSummaryByCurrency(t *testing.T) {
	svc := newApp(t)
	createPayment(t, svc, "Аренда", "2026-11-01", 45000, domain.RUB)
	createPayment(t, svc, "Сервер", "2026-11-05", 120, domain.USD)
	createPayment(t, svc, "Подписка", "2026-11-10", 8.75, domain.EUR)

	sum, err := svc.Summary()
	if err != nil {
		t.Fatal(err)
	}

	got := map[domain.Currency]float64{}
	for _, c := range sum.ByCurrency {
		got[c.Currency] = c.Total
	}
	if got[domain.RUB] != 540000 || got[domain.USD] != 1440 || got[domain.EUR] != 105 {
		t.Fatalf("итоги по валютам: %+v", got)
	}
	if _, mixed := got[""]; mixed {
		t.Fatal("валюты не должны складываться в общую сумму")
	}
}

// Вторая попытка занять занятый логин даёт доменную ошибку, а не сырой SQL.
func TestDuplicateLogin(t *testing.T) {
	svc := newApp(t)

	if _, err := svc.CreateUser("ivan", "secret123"); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CreateUser("ivan", "secret123")
	if !errors.Is(err, domain.ErrDuplicateLogin) {
		t.Fatalf("ожидалась ErrDuplicateLogin, получено %v", err)
	}
}

// Пользователь создаётся с ролью user и не может получить права админа.
func TestCreatedUserIsNotAdmin(t *testing.T) {
	svc := newApp(t)

	u, err := svc.CreateUser("ivan", "secret123")
	if err != nil {
		t.Fatal(err)
	}
	if u.IsAdmin() {
		t.Fatal("созданный через сервис пользователь не должен быть администратором")
	}
}

// Короткий пароль и пустой логин отвергаются до записи в БД.
func TestUserValidation(t *testing.T) {
	svc := newApp(t)

	if _, err := svc.CreateUser("", "secret123"); err == nil {
		t.Fatal("пустой логин должен быть отвергнут")
	}
	if _, err := svc.CreateUser("ivan", "123"); err == nil {
		t.Fatal("короткий пароль должен быть отвергнут")
	}
}

// Отозванная сессия перестаёт действовать — logout работает на уровне сервиса.
func TestSessionRevocation(t *testing.T) {
	svc := newApp(t)

	jti := domain.NewID()
	revoked, err := svc.SessionRevoked(jti)
	if err != nil {
		t.Fatal(err)
	}
	if revoked {
		t.Fatal("новая сессия не должна быть отозвана")
	}

	if err := svc.RevokeSession(jti, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	revoked, err = svc.SessionRevoked(jti)
	if err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("после logout сессия должна быть отозвана")
	}
}
