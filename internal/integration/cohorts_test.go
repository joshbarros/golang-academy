package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-academy/internal/cohorts"
	"github.com/joshbarros/golang-academy/internal/platform/messaging"
)

func (e *env) rawCohort(seats int) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	_, err := e.pool.Exec(context.Background(), `INSERT INTO cohorts.cohorts (id, code, title, track, instructor, starts_at, seat_capacity, seats_left, created_at)
		VALUES ($1,$2,'a','b','c',now(),$3,$3,now())`, id, code(), seats)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func reserve(e *env, cohort uuid.UUID, seats int) cohorts.ReserveOutcome {
	out, err := e.app.Cohorts.Reserve(context.Background(), uuid.New(), messaging.ReserveSeats{EnrollmentID: uuid.New(), OwnerID: uuid.New(), CohortID: cohort, Seats: seats, Reference: "R"})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestConcurrentReservesNeverOversellAndAlwaysSumToTheStock(t *testing.T) {
	e := newEnv(t)
	cohort := e.rawCohort(30)
	var wg sync.WaitGroup
	var mu sync.Mutex
	held := 0
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reserve(e, cohort, 1) == cohorts.Held {
				mu.Lock()
				held++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	var left int
	_ = e.pool.QueryRow(context.Background(), `SELECT seats_left FROM cohorts.cohorts WHERE id = $1`, cohort).Scan(&left)
	if held != 30 || left != 0 {
		t.Fatalf("60 enrollments, 30 seats: exactly 30 held and none left, got %d held and %d left", held, left)
	}
	if e.count(`SELECT count(*) FROM cohorts.seat_holds WHERE cohort_id = $1`, cohort) != 30 {
		t.Fatal("one hold per success")
	}
}

func TestAShortReserveWritesNoHoldAndTakesNothing(t *testing.T) {
	e := newEnv(t)
	cohort := e.rawCohort(3)
	if reserve(e, cohort, 2) != cohorts.Held || reserve(e, cohort, 2) != cohorts.Short {
		t.Fatal("2 of 3 held, then 2 wanted and 1 left is short")
	}
	var left int
	_ = e.pool.QueryRow(context.Background(), `SELECT seats_left FROM cohorts.cohorts WHERE id = $1`, cohort).Scan(&left)
	if left != 1 || e.count(`SELECT count(*) FROM cohorts.seat_holds WHERE cohort_id = $1`, cohort) != 1 {
		t.Fatalf("the short try left no trace: %d left", left)
	}
	if reserve(e, cohort, 1) != cohorts.Held {
		t.Fatal("the last seat is still there")
	}
}

func TestReleaseGivesBackExactlyOnce(t *testing.T) {
	e := newEnv(t)
	cohort := e.rawCohort(5)
	enrollment := uuid.New()
	if out, _ := e.app.Cohorts.Reserve(context.Background(), uuid.New(), messaging.ReserveSeats{EnrollmentID: enrollment, OwnerID: uuid.New(), CohortID: cohort, Seats: 2, Reference: "R"}); out != cohorts.Held {
		t.Fatal(out)
	}
	for i := 0; i < 3; i++ {
		if _, err := e.app.Cohorts.Release(context.Background(), uuid.New(), messaging.ReleaseSeats{EnrollmentID: enrollment, Reason: "refund"}); err != nil {
			t.Fatal(err)
		}
	}
	var left int
	_ = e.pool.QueryRow(context.Background(), `SELECT seats_left FROM cohorts.cohorts WHERE id = $1`, cohort).Scan(&left)
	if left != 5 || e.outbox("cohorts", messaging.ReleasedKey) != 1 {
		t.Fatalf("given back once, not three times: %d left", left)
	}
}

func TestOpeningACohortValidatesIt(t *testing.T) {
	e := newEnv(t)
	adm := e.admin()
	good := func() map[string]any {
		return map[string]any{"code": code(), "title": "a", "track": "b", "instructor": "c", "seatCapacity": 10}
	}
	if r := e.post("/api/cohorts", good(), adm.Token); r.Status != 201 || r.Body["seatsLeft"] != float64(10) {
		t.Fatalf("a cohort opens full: %d %s", r.Status, r.Raw)
	}
	for name, mut := range map[string]func(map[string]any){
		"no code":   func(m map[string]any) { m["code"] = "" },
		"long code": func(m map[string]any) { m["code"] = "ABCDEFGHIJKLMNOPQRSTU" },
		"no text":   func(m map[string]any) { m["track"] = " " },
		"zero":      func(m map[string]any) { m["seatCapacity"] = 0 },
		"too many":  func(m map[string]any) { m["seatCapacity"] = 100_000_000 },
	} {
		bad := good()
		mut(bad)
		if r := e.post("/api/cohorts", bad, adm.Token); r.Status != 400 {
			t.Errorf("%s: %d", name, r.Status)
		}
	}
	dup := good()
	e.post("/api/cohorts", dup, adm.Token)
	if r := e.post("/api/cohorts", dup, adm.Token); r.Status != 409 {
		t.Fatalf("the same code twice: %d", r.Status)
	}
	if r := e.post("/api/cohorts", good(), e.register().Token); r.Status != 403 {
		t.Fatalf("a customer cannot open a cohort: %d", r.Status)
	}
}
