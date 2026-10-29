package enrollments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-academy/internal/cohorts"
	"github.com/joshbarros/golang-academy/internal/platform/httpx"
	"github.com/joshbarros/golang-academy/internal/platform/messaging"
	"github.com/joshbarros/golang-academy/internal/platform/pgdb"
)

// Schema and consumer names.
const (
	Schema       = "enrollments"
	ConsumerName = "enrollments"
)

// Enrollment statuses.
const (
	Requested = "Requested"
	Held      = "Held"
	Enrolled  = "Enrolled"
	Withdrawn = "Withdrawn"
	Rejected  = "Rejected"
)

// Enrollment is the public view of an enrollment.
type Enrollment struct {
	ID              uuid.UUID  `json:"id"`
	OwnerID         uuid.UUID  `json:"ownerId"`
	CohortCode      string     `json:"cohortCode"`
	Reference       string     `json:"reference"`
	Seats           int        `json:"seats"`
	FeeCents        int64      `json:"feeCents"`
	Status          string     `json:"status"`
	Reason          *string    `json:"reason"`
	PassNumber      *string    `json:"passNumber"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	SeatsReleasedAt *time.Time `json:"seatsReleasedAt"`
}

// CohortCatalog is the one thing Enrollments may ask the cohorts module.
type CohortCatalog interface {
	Find(ctx context.Context, code string) (*cohorts.Info, error)
}

// Service is the HTTP side of the saga: it writes the enrollment and its first outbox row in one local transaction.
type Service struct {
	Pool    *pgxpool.Pool
	Outbox  messaging.Outbox
	Cohorts CohortCatalog
	Log     *slog.Logger

	// FinalMarkDelay is a latency knob for the final mark: Kubernetes sets it so the refund race window is wide enough to hit on purpose.
	FinalMarkDelay time.Duration
}

// NewService builds a Service with its outbox.
func NewService(pool *pgxpool.Pool, catalog CohortCatalog, log *slog.Logger) *Service {
	return &Service{Pool: pool, Outbox: messaging.Outbox{Schema: Schema, Module: "enrollments"}, Cohorts: catalog, Log: log}
}

const enrollmentColumns = `id, owner_id, cohort_code, reference, seats, fee_cents, status, reason, pass_number, created_at, updated_at, seats_released_at`

func scanEnrollment(row pgx.Row) (Enrollment, error) {
	var o Enrollment
	err := row.Scan(&o.ID, &o.OwnerID, &o.CohortCode, &o.Reference, &o.Seats, &o.FeeCents, &o.Status, &o.Reason, &o.PassNumber, &o.CreatedAt, &o.UpdatedAt, &o.SeatsReleasedAt)
	return o, err
}

// CreateRequest is the body of POST /api/enrollments.
type CreateRequest struct {
	CohortCode string `json:"cohortCode"`
	Reference  string `json:"reference"`
	Seats      int64  `json:"seats"`
}

// Create reserves seats in the cohort for the caller: validates, writes the enrollment and its reserve command in one transaction.
func (s *Service) Create(ctx context.Context, caller *httpx.Principal, req CreateRequest) (Enrollment, error) {
	if caller == nil {
		return Enrollment{}, httpx.Fail(http.StatusUnauthorized, "Sign in to book.")
	}
	if strings.TrimSpace(req.CohortCode) == "" {
		return Enrollment{}, httpx.Fail(http.StatusBadRequest, "Cohort code is required.")
	}
	if msg := Validate(req.Reference, req.Seats); msg != "" {
		return Enrollment{}, httpx.Fail(http.StatusBadRequest, "%s", msg)
	}
	cohort, err := s.Cohorts.Find(ctx, strings.TrimSpace(req.CohortCode))
	if err != nil {
		return Enrollment{}, err
	}
	if cohort == nil {
		return Enrollment{}, httpx.Fail(http.StatusNotFound, "Cohort '%s' does not exist.", req.CohortCode)
	}
	now := time.Now().UTC()
	seats := int(req.Seats)
	reference := strings.TrimSpace(req.Reference)
	fee, err := FeeCents(seats)
	if err != nil {
		return Enrollment{}, httpx.Fail(http.StatusBadRequest, "The fee is too large.")
	}
	id, _ := uuid.NewV7()
	o := Enrollment{ID: id, OwnerID: caller.ID, CohortCode: cohort.Code, Reference: reference, Seats: seats, FeeCents: fee, Status: Requested, CreatedAt: now, UpdatedAt: now}
	err = pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO enrollments.enrollments (id, owner_id, cohort_id, cohort_code, reference, seats, fee_cents, status, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'Requested',$8,$8)`, o.ID, o.OwnerID, cohort.ID, o.CohortCode, o.Reference, o.Seats, o.FeeCents, now); err != nil {
			return err
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.ReserveKey, o.ID,
			messaging.ReserveSeats{EnrollmentID: o.ID, OwnerID: caller.ID, CohortID: cohort.ID, Seats: seats, Reference: reference})
	})
	if err != nil {
		if pgdb.IsUniqueViolation(err) {
			return Enrollment{}, httpx.Fail(http.StatusConflict, "Reference '%s' is already booked on %s.", reference, cohort.Code)
		}
		return Enrollment{}, err
	}
	return o, nil
}

// Get returns an enrollment to its owner or to staff. Anyone else gets a 404, so ids cannot be probed.
func (s *Service) Get(ctx context.Context, caller *httpx.Principal, id uuid.UUID) (Enrollment, error) {
	o, err := scanEnrollment(s.Pool.QueryRow(ctx, `SELECT `+enrollmentColumns+` FROM enrollments.enrollments WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !caller.CanAccess(o.OwnerID)) {
		return Enrollment{}, httpx.Fail(http.StatusNotFound, "Enrollment not found.")
	}
	return o, err
}

// List returns the caller's enrollments (staff: everyone's), newest first, a page at a time, with the total.
func (s *Service) List(ctx context.Context, caller *httpx.Principal, cohortCode string, page, pageSize int) ([]Enrollment, int, error) {
	if caller == nil {
		return nil, 0, httpx.Fail(http.StatusUnauthorized, "Sign in to see enrollments.")
	}
	where := []string{"true"}
	args := []any{}
	if !caller.IsStaff() {
		args = append(args, caller.ID)
		where = append(where, "owner_id = $"+strconv.Itoa(len(args)))
	}
	if strings.TrimSpace(cohortCode) != "" {
		args = append(args, cohortCode)
		where = append(where, "cohort_code = $"+strconv.Itoa(len(args)))
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM enrollments.enrollments WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := s.Pool.Query(ctx, fmt.Sprintf(`SELECT %s FROM enrollments.enrollments WHERE %s ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`,
		enrollmentColumns, cond, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Enrollment{}
	for rows.Next() {
		o, err := scanEnrollment(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, o)
	}
	return items, total, rows.Err()
}

// Refund moves Held or Enrolled to Withdrawn and asks Cohorts to give the seats back. The pass is not touched.
func (s *Service) Refund(ctx context.Context, caller *httpx.Principal, id uuid.UUID) (Enrollment, error) {
	if _, err := s.Get(ctx, caller, id); err != nil { // 404 unless the caller may see it
		return Enrollment{}, err
	}
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE enrollments.enrollments SET status = 'Withdrawn', reason = 'Withdrawn by request.', updated_at = now()
			WHERE id = $1 AND status IN ('Held','Enrolled')`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			var current string
			if err := tx.QueryRow(ctx, `SELECT status FROM enrollments.enrollments WHERE id = $1`, id).Scan(&current); err != nil {
				return err
			}
			return httpx.Fail(http.StatusConflict, "An enrollment in status %s cannot be withdrawn.", current)
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.ReleaseKey, id, messaging.ReleaseSeats{EnrollmentID: id, Reason: "refund"})
	})
	if err != nil {
		return Enrollment{}, err
	}
	return scanEnrollment(s.Pool.QueryRow(ctx, `SELECT `+enrollmentColumns+` FROM enrollments.enrollments WHERE id = $1`, id))
}
