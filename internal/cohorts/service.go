package cohorts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-academy/internal/platform/messaging"
	"github.com/joshbarros/golang-academy/internal/platform/pgdb"
)

// Schema and consumer names.
const (
	Schema       = "cohorts"
	ConsumerName = "cohorts"

	HoldHeld     = "Held"
	HoldReleased = "Released"
)

// ReserveOutcome is what a reserve did.
type ReserveOutcome int

// The ways a reserve can end.
const (
	Held ReserveOutcome = iota
	Short
	Duplicate
)

// Service is the cohorts module's logic. It is the only writer of seats_left.
type Service struct {
	Pool   *pgxpool.Pool
	Outbox messaging.Outbox
	Log    *slog.Logger
}

// NewService builds a Service with its outbox.
func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{Pool: pool, Outbox: messaging.Outbox{Schema: Schema, Module: "cohorts"}, Log: log}
}

// Handle implements messaging.Handler for the cohorts queue.
func (s *Service) Handle(ctx context.Context, d messaging.Delivery) error {
	switch d.RoutingKey {
	case messaging.ReserveKey:
		cmd, err := messaging.Parse[messaging.ReserveSeats](d.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
		}
		_, err = s.Reserve(ctx, d.MessageID, cmd)
		return err
	case messaging.ReleaseKey:
		cmd, err := messaging.Parse[messaging.ReleaseSeats](d.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
		}
		_, err = s.Release(ctx, d.MessageID, cmd)
		return err
	}
	return fmt.Errorf("cohorts does not handle %q", d.RoutingKey)
}

// Reserve holds seats with one conditional UPDATE that matches only while the remaining seats still cover the request: when
// two enrollments fight for the last seat, one commits a hold and the other is short and writes none.
func (s *Service) Reserve(ctx context.Context, messageID uuid.UUID, cmd messaging.ReserveSeats) (ReserveOutcome, error) {
	outcome := Held
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		tag, err := tx.Exec(ctx, `UPDATE cohorts.cohorts SET seats_left = seats_left - $2 WHERE id = $1 AND seats_left >= $2`, cmd.CohortID, cmd.Seats)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			if _, err := tx.Exec(ctx, `INSERT INTO cohorts.seat_holds (enrollment_id, owner_id, cohort_id, seats, status, created_at) VALUES ($1,$2,$3,$4,'Held',now())`,
				cmd.EnrollmentID, cmd.OwnerID, cmd.CohortID, cmd.Seats); err != nil {
				return err
			}
			return s.Outbox.Enqueue(ctx, tx, messaging.HeldKey, cmd.EnrollmentID,
				messaging.SeatsHeld{EnrollmentID: cmd.EnrollmentID, OwnerID: cmd.OwnerID, CohortID: cmd.CohortID, Seats: cmd.Seats, Reference: cmd.Reference})
		}
		outcome = Short
		var left int
		_ = tx.QueryRow(ctx, `SELECT seats_left FROM cohorts.cohorts WHERE id = $1`, cmd.CohortID).Scan(&left)
		return s.Outbox.Enqueue(ctx, tx, messaging.ShortKey, cmd.EnrollmentID,
			messaging.SeatsShort{EnrollmentID: cmd.EnrollmentID, Reference: cmd.Reference, RequestedSeats: cmd.Seats, SeatsLeft: left})
	})
	return outcome, err
}

// Release gives held seats back exactly once. A second release for the same enrollment changes nothing.
func (s *Service) Release(ctx context.Context, messageID uuid.UUID, cmd messaging.ReleaseSeats) (bool, error) {
	released := false
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil || !first {
			return err
		}
		var cohortID uuid.UUID
		var seats int
		err = tx.QueryRow(ctx, `
			UPDATE cohorts.seat_holds SET status = 'Released', released_at = now(), release_reason = $2
			WHERE enrollment_id = $1 AND status = 'Held' RETURNING cohort_id, seats`, cmd.EnrollmentID, cmd.Reason).Scan(&cohortID, &seats)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // nothing held, or already released
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE cohorts.cohorts SET seats_left = seats_left + $2 WHERE id = $1 AND seats_left + $2 <= seat_capacity`, cohortID, seats); err != nil {
			return err
		}
		released = true
		return s.Outbox.Enqueue(ctx, tx, messaging.ReleasedKey, cmd.EnrollmentID, messaging.SeatsReleased{EnrollmentID: cmd.EnrollmentID, Seats: seats, Reason: cmd.Reason})
	})
	return released, err
}
