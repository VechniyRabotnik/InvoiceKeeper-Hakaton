package domain_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aeza/invoice-keeper/internal/domain"
)

// Ключевое правило кейса: день месяца — якорь, а не «уехавшая» дата.
// 31 января → 28/29 февраля → снова 31 марта.
func TestDueDateKeepsDayAnchor(t *testing.T) {
	first, err := domain.ParseDay("2026-01-31")
	if err != nil {
		t.Fatal(err)
	}
	p := domain.RecurringPayment{FirstDue: first, Period: domain.Monthly}

	want := []string{"2026-01-31", "2026-02-28", "2026-03-31", "2026-04-30"}
	for n, exp := range want {
		if got := p.DueDate(n).String(); got != exp {
			t.Errorf("период %d: получено %s, ожидалось %s", n, got, exp)
		}
	}
}

func TestDueDateLeapFebruary(t *testing.T) {
	first, _ := domain.ParseDay("2028-01-31") // 2028 — високосный
	p := domain.RecurringPayment{FirstDue: first, Period: domain.Monthly}

	if got := p.DueDate(1).String(); got != "2028-02-29" {
		t.Errorf("февраль високосного года: получено %s", got)
	}
	if got := p.DueDate(2).String(); got != "2028-03-31" {
		t.Errorf("возврат к якорю: получено %s", got)
	}
}

func TestDueDateOtherPeriods(t *testing.T) {
	first, _ := domain.ParseDay("2026-01-15")
	cases := []struct {
		period domain.Period
		n      int
		want   string
	}{
		{domain.Weekly, 3, "2026-02-05"},
		{domain.Quarterly, 2, "2026-07-15"},
		{domain.HalfYear, 2, "2027-01-15"},
		{domain.Yearly, 3, "2029-01-15"},
	}
	for _, c := range cases {
		p := domain.RecurringPayment{FirstDue: first, Period: c.period}
		if got := p.DueDate(c.n).String(); got != c.want {
			t.Errorf("%s n=%d: получено %s, ожидалось %s", c.period, c.n, got, c.want)
		}
	}
}

// Просрочка начинается со следующего дня после срока: оплата в день срока —
// вовремя, а на следующий день период уже overdue.
func TestResolveStatus(t *testing.T) {
	due, _ := domain.ParseDay("2026-11-01")
	paidAt, _ := domain.ParseDay("2026-11-05")

	day := func(s string) domain.Day { d, _ := domain.ParseDay(s); return d }

	cases := []struct {
		name   string
		paidAt *domain.Day
		today  domain.Day
		want   domain.Status
	}{
		{"до срока", nil, day("2026-10-31"), domain.StatusPlanned},
		{"в день срока", nil, day("2026-11-01"), domain.StatusDue},
		{"на следующий день", nil, day("2026-11-02"), domain.StatusOverdue},
		{"оплачен заранее", &paidAt, day("2026-10-31"), domain.StatusPaid},
		{"оплачен с опозданием", &paidAt, day("2026-12-01"), domain.StatusPaid},
	}
	for _, c := range cases {
		o := domain.Occurrence{DueDate: due, PaidAt: c.paidAt}
		o.ResolveStatus(c.today)
		if o.Status != c.want {
			t.Errorf("%s: получено %s, ожидалось %s", c.name, o.Status, c.want)
		}
	}
}

// Исправление фактической даты не меняет срок, но пересчитывает статус.
func TestStatusRecalculatedAfterDateFix(t *testing.T) {
	due, _ := domain.ParseDay("2026-11-01")
	wrong, _ := domain.ParseDay("2026-11-20")
	right, _ := domain.ParseDay("2026-11-02")

	o := domain.Occurrence{DueDate: due, PaidAt: &wrong}
	o.ResolveStatus(domain.Day{})
	if o.Status != domain.StatusPaid {
		t.Fatalf("оплаченный период должен быть paid, получено %s", o.Status)
	}

	// Снимаем отметку — просрочка возвращается из дат, а не из флага.
	o.PaidAt = nil
	o.ResolveStatus(mustDay(t, "2026-11-10"))
	if o.Status != domain.StatusOverdue {
		t.Fatalf("без отметки период должен быть overdue, получено %s", o.Status)
	}

	// Исправленная дата снова делает период оплаченным.
	o.PaidAt = &right
	o.ResolveStatus(mustDay(t, "2026-11-10"))
	if o.Status != domain.StatusPaid {
		t.Fatalf("после исправления даты ожидался paid, получено %s", o.Status)
	}
	if o.DueDate.String() != "2026-11-01" {
		t.Fatalf("срок не должен меняться, получено %s", o.DueDate)
	}
}

// Суммы хранятся в минорных единицах: 0.1+0.2 не даёт погрешности.
func TestAmountPrecision(t *testing.T) {
	sum := domain.ParseAmount(0.1) + domain.ParseAmount(0.2)
	if got := sum.Major(); got != 0.3 {
		t.Errorf("0.1 + 0.2 = %v, ожидалось 0.3", got)
	}

	raw, err := json.Marshal(domain.ParseAmount(45000))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "45000.00" {
		t.Errorf("JSON суммы: %s, ожидалось 45000.00", raw)
	}
}

// В API даты — календарные строки, без времени и без сдвига пояса.
func TestDayJSONRoundTrip(t *testing.T) {
	d, err := domain.ParseDay("2026-11-01")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `"2026-11-01"` {
		t.Fatalf("сериализация: %s", raw)
	}

	var back domain.Day
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Equal(d) {
		t.Fatalf("round-trip: получено %s", back)
	}

	var invalid domain.Day
	if err := json.Unmarshal([]byte(`"01.11.2026"`), &invalid); err == nil {
		t.Fatal("ожидалась ошибка для неверного формата даты")
	}
}

func TestPaymentValidation(t *testing.T) {
	due, _ := domain.ParseDay("2026-11-01")
	valid := domain.RecurringPayment{
		Name: "Аренда", Amount: 45000, Currency: domain.RUB,
		Period: domain.Monthly, FirstDue: due,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("корректный платёж отвергнут: %v", err)
	}

	cases := map[string]func(*domain.RecurringPayment){
		"без названия":     func(p *domain.RecurringPayment) { p.Name = "" },
		"нулевая сумма":    func(p *domain.RecurringPayment) { p.Amount = 0 },
		"чужая валюта":     func(p *domain.RecurringPayment) { p.Currency = "GBP" },
		"неизвестный срок": func(p *domain.RecurringPayment) { p.Period = "daily" },
	}
	for name, mutate := range cases {
		p := valid
		mutate(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: ожидалась ошибка валидации", name)
		}
	}
}

func mustDay(t *testing.T, s string) domain.Day {
	t.Helper()
	d, err := domain.ParseDay(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Календарные операции не должны зависеть от перехода на летнее время.
func TestDayAddMonthsAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Skip("нет базы часовых поясов")
	}
	d := domain.NewDay(time.Date(2026, 3, 31, 12, 0, 0, 0, loc), loc)
	if got := d.AddMonths(1).String(); got != "2026-04-30" {
		t.Errorf("31 марта + 1 месяц: получено %s", got)
	}
}
