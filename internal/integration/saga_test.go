package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-academy/internal/enrollments"
	"github.com/joshbarros/golang-academy/internal/passes"
	"github.com/joshbarros/golang-academy/internal/platform/messaging"
)

func (e *env) outbox(schema, key string) int {
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+schema+`.outbox_messages WHERE routing_key = $1`, key).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) count(sql string, args ...any) int {
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) seatsLeft(cohortCode string) float64 {
	e.t.Helper()
	v, _ := e.get("/api/cohorts/"+cohortCode, e.admin().Token).Body["seatsLeft"].(float64)
	return v
}

func status(e *env, s session, id string) string { return e.enrollment(s, id).str("status") }

func feeOf(t *testing.T, seats int) float64 {
	t.Helper()
	fee, err := enrollments.FeeCents(seats)
	if err != nil {
		t.Fatal(err)
	}
	return float64(fee)
}

func TestTheWholeSagaFromEnrollmentToEnrolledAndThePass(t *testing.T) {
	e := newEnv(t)
	cohort := e.openCohort(10)
	ana := e.register()
	created := e.enter(ana, cohort, "COL-1", 2)
	id := created.str("id")
	if created.Status != 202 || created.str("status") != "Requested" || created.Body["feeCents"] != feeOf(t, 2) {
		t.Fatalf("accepted as Requested with the fee: %s", created.Raw)
	}
	if dead := e.drain(); dead != 0 {
		t.Fatalf("nothing should dead-letter: %d", dead)
	}
	o := e.enrollment(ana, id)
	if o.str("status") != "Enrolled" || !strings.HasPrefix(o.str("passNumber"), "AP-") || o.Body["reason"] != nil {
		t.Fatalf("sold with a pass number and no leftover reason: %s", o.Raw)
	}
	if e.seatsLeft(cohort) != 8 {
		t.Fatal("2 of 10 seats are held")
	}
	c := e.get("/api/passes/"+id, ana.Token)
	if c.Status != 200 || c.str("status") != "Issued" || c.str("number") != o.str("passNumber") {
		t.Fatalf("the pass: %d %s", c.Status, c.Raw)
	}
}

func TestWhenTwoEnrollmentsFightForTheLastSeatsOneCommitsAndTheOtherIsShortAndWritesNoHold(t *testing.T) {
	e := newEnv(t)
	cohort := e.openCohort(3)
	ana, bob := e.register(), e.register()
	a := e.enter(ana, cohort, "COL-A", 2).str("id")
	e.drain()
	b := e.enter(bob, cohort, "COL-B", 2).str("id")
	e.drain()
	if status(e, ana, a) != "Enrolled" {
		t.Fatal("the first enrollment is sold")
	}
	o := e.enrollment(bob, b)
	if o.str("status") != "Rejected" || o.str("reason") != "Short: 2 seats requested, 1 remaining." {
		t.Fatalf("the second is short: %s", o.Raw)
	}
	if e.get("/api/passes/"+b, bob.Token).Status != 404 || e.count(`SELECT count(*) FROM cohorts.seat_holds`) != 1 {
		t.Fatal("no pass and no hold for a refused enrollment")
	}
	if e.seatsLeft(cohort) != 1 {
		t.Fatal("the refused enrollment took nothing")
	}
}

func TestPoisonIsRejectedAndTheSeatsComeBack(t *testing.T) {
	e := newEnv(t)
	cohort := e.openCohort(5)
	ana := e.register()
	id := e.enter(ana, cohort, "COL-POISON-1", 2).str("id")
	if dead := e.drain(); dead != 1 {
		t.Fatalf("the poison pass is dead-lettered once: %d", dead)
	}
	o := e.enrollment(ana, id)
	if o.str("status") != "Rejected" || !strings.Contains(o.str("reason"), "poison") || o.Body["seatsReleasedAt"] == nil {
		t.Fatalf("rejected, with the reason, and the release recorded: %s", o.Raw)
	}
	if e.seatsLeft(cohort) != 5 {
		t.Fatal("the seats are back")
	}
	if c := e.get("/api/passes/"+id, ana.Token); c.str("status") != "Rejected" {
		t.Fatalf("the rejection is committed on the pass: %s", c.Raw)
	}
}

func TestRefundAfterSaleReturnsTheSeatsButKeepsThePass(t *testing.T) {
	e := newEnv(t)
	cohort := e.openCohort(5)
	ana := e.register()
	id := e.enter(ana, cohort, "COL-1", 2).str("id")
	e.drain()
	if r := e.post("/api/enrollments/"+id+"/withdrawal", nil, ana.Token); r.Status != 202 {
		t.Fatalf("refund: %d %s", r.Status, r.Raw)
	}
	e.drain()
	if status(e, ana, id) != "Withdrawn" || e.seatsLeft(cohort) != 5 {
		t.Fatal("refunded and the seats are back on the shelf")
	}
	if c := e.get("/api/passes/"+id, ana.Token); c.str("status") != "Issued" {
		t.Fatal("a refund does not roll back the pass that was issued")
	}
	if r := e.post("/api/enrollments/"+id+"/withdrawal", nil, ana.Token); r.Status != 409 {
		t.Fatalf("a second refund is a conflict: %d", r.Status)
	}
}

func TestRefundBeatsAFinalMarkThatArrivesLate(t *testing.T) {
	e := newEnv(t)
	cohort := e.openCohort(5)
	ana := e.register()
	id := uuid.MustParse(e.enter(ana, cohort, "COL-1", 1).str("id"))
	for pass := 0; pass < 4; pass++ {
		for _, row := range e.unsent() {
			if row.d.RoutingKey == messaging.IssuedKey {
				continue
			}
			e.markSent(row)
			_, _ = e.deliver(row.d.RoutingKey, row.d)
		}
	}
	if got := status(e, ana, id.String()); got != "Held" {
		t.Fatalf("held, pass issued, final mark not yet landed: %s", got)
	}
	if r := e.post("/api/enrollments/"+id.String()+"/withdrawal", nil, ana.Token); r.Status != 202 {
		t.Fatal(r.Status)
	}
	e.drain()
	o := e.enrollment(ana, id.String())
	if o.str("status") != "Withdrawn" || o.Body["passNumber"] != nil {
		t.Fatalf("the late final mark must not undo the refund: %s", o.Raw)
	}
	if c := e.get("/api/passes/"+id.String(), ana.Token); c.str("status") != "Issued" {
		t.Fatal("the issued pass stays")
	}
}

func TestADeliveryRepeatedByTheBrokerHasNoEffectTheSecondTime(t *testing.T) {
	e := newEnv(t)
	cohort := e.openCohort(5)
	ana := e.register()
	e.enter(ana, cohort, "COL-1", 1)
	rows := e.unsent()
	if len(rows) != 1 {
		t.Fatal("one reserve command")
	}
	d := rows[0].d
	for i := 0; i < 3; i++ {
		if _, err := e.deliver(d.RoutingKey, d); err != nil {
			t.Fatal(err)
		}
	}
	if e.count(`SELECT count(*) FROM cohorts.seat_holds`) != 1 || e.outbox("cohorts", messaging.HeldKey) != 1 || e.seatsLeft(cohort) != 4 {
		t.Fatal("at-least-once delivery, exactly-once effect: the inbox absorbs the repeats")
	}
}

func TestAFinalMarkBeforeTheHeldMarkIsRetriedNotCohortped(t *testing.T) {
	e := newEnv(t)
	cohort := e.openCohort(5)
	ana := e.register()
	id := uuid.MustParse(e.enter(ana, cohort, "COL-1", 1).str("id"))
	_, err := e.app.Enrollments.MarkEnrolled(context.Background(), uuid.New(), messaging.PassIssued{EnrollmentID: id, Number: "AP-TEST"}, 0)
	var ooo *messaging.OutOfEnrollmentError
	if err == nil || !errorsAs(err, &ooo) {
		t.Fatalf("the final mark must ask to be retried, got %v", err)
	}
	if status(e, ana, id.String()) != "Requested" {
		t.Fatal("and change nothing")
	}
	e.drain()
	if status(e, ana, id.String()) != "Enrolled" {
		t.Fatal("the normal flow still completes")
	}
}

func TestIssueIsIdempotentAndPoisonIsAFlagNotAnException(t *testing.T) {
	e := newEnv(t)
	id, owner := uuid.New(), uuid.New()
	msg := uuid.New()
	held := messaging.SeatsHeld{EnrollmentID: id, OwnerID: owner, CohortID: uuid.New(), Seats: 1, Reference: "COL-1"}
	first, err := e.app.Passes.Issue(context.Background(), msg, held)
	again, err2 := e.app.Passes.Issue(context.Background(), msg, held)
	if err != nil || err2 != nil || first != passes.Issued || again != passes.Duplicate {
		t.Fatalf("issue then duplicate: %v %v %v %v", first, err, again, err2)
	}
	poison := messaging.SeatsHeld{EnrollmentID: uuid.New(), OwnerID: owner, CohortID: uuid.New(), Seats: 1, Reference: "xPOISONx"}
	if out, _ := e.app.Passes.Issue(context.Background(), uuid.New(), poison); out != passes.Rejected {
		t.Fatal("a poison reference is rejected")
	}
	if e.outbox("passes", messaging.ReleaseKey) != 1 || e.outbox("passes", messaging.RejectedKey) != 1 || e.outbox("passes", messaging.IssuedKey) != 1 {
		t.Fatal("one issued, one release command and one rejection")
	}
}
