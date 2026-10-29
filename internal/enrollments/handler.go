package enrollments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/joshbarros/golang-academy/internal/platform/messaging"
	"github.com/joshbarros/golang-academy/internal/platform/pgdb"
)

// MarkOutcome is what a mark did.
type MarkOutcome int

// The ways a mark can end.
const (
	Marked MarkOutcome = iota
	Skipped
	Duplicate
)

// Handle consumes the events that move an enrollment forward. Each step runs only from the status it expects.
func (s *Service) Handle(ctx context.Context, d messaging.Delivery) error {
	poison := func(err error) error { return fmt.Errorf("%w: %v", messaging.ErrPoison, err) }
	switch d.RoutingKey {
	case messaging.HeldKey:
		m, err := messaging.Parse[messaging.SeatsHeld](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkHeld(ctx, d.MessageID, m.EnrollmentID)
		return err
	case messaging.ShortKey:
		m, err := messaging.Parse[messaging.SeatsShort](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkShort(ctx, d.MessageID, m)
		return err
	case messaging.IssuedKey:
		m, err := messaging.Parse[messaging.PassIssued](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkEnrolled(ctx, d.MessageID, m, s.FinalMarkDelay)
		return err
	case messaging.RejectedKey:
		m, err := messaging.Parse[messaging.PassRejected](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkRejected(ctx, d.MessageID, m)
		return err
	case messaging.ReleasedKey:
		m, err := messaging.Parse[messaging.SeatsReleased](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkReleased(ctx, d.MessageID, m)
		return err
	}
	return fmt.Errorf("enrollments does not handle %q", d.RoutingKey)
}

// step runs update inside one transaction with the inbox pass. When update changes nothing and the enrollment is still waiting
// for an earlier mark (Requested), the message arrived too early: it is retried, not dropped.
func (s *Service) step(ctx context.Context, messageID uuid.UUID, enrollmentForEnrollmentCheck *uuid.UUID, update func(pgx.Tx) (int64, error)) (MarkOutcome, error) {
	outcome := Marked
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		changed, err := update(tx)
		if err != nil {
			return err
		}
		if changed == 0 && enrollmentForEnrollmentCheck != nil {
			var status string
			err := tx.QueryRow(ctx, `SELECT status FROM enrollments.enrollments WHERE id = $1`, *enrollmentForEnrollmentCheck).Scan(&status)
			if err == nil && status == Requested {
				return &messaging.OutOfEnrollmentError{Reason: "The final mark arrived before the Held mark."}
			}
		}
		if changed == 0 {
			outcome = Skipped
		}
		return nil
	})
	return outcome, err
}

// MarkHeld: the seats are held.
func (s *Service) MarkHeld(ctx context.Context, messageID, enrollmentID uuid.UUID) (MarkOutcome, error) {
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE enrollments.enrollments SET status = 'Held', reason = NULL, updated_at = now() WHERE id = $1 AND status = 'Requested'`, enrollmentID)
		return tag.RowsAffected(), err
	})
}

// ShortReason words why the reserve was refused.
func ShortReason(m messaging.SeatsShort) string {
	return fmt.Sprintf("Short: %d seats requested, %d remaining.", m.RequestedSeats, m.SeatsLeft)
}

// MarkShort: the stock said no.
func (s *Service) MarkShort(ctx context.Context, messageID uuid.UUID, m messaging.SeatsShort) (MarkOutcome, error) {
	reason := ShortReason(m)
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE enrollments.enrollments SET status = 'Rejected', reason = $2, updated_at = now() WHERE id = $1 AND status = 'Requested'`, m.EnrollmentID, reason)
		return tag.RowsAffected(), err
	})
}

// MarkEnrolled is the final mark. If a refund got there first the row is no longer Held and nothing changes.
func (s *Service) MarkEnrolled(ctx context.Context, messageID uuid.UUID, m messaging.PassIssued, delay time.Duration) (MarkOutcome, error) {
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return Skipped, ctx.Err()
		}
	}
	return s.step(ctx, messageID, &m.EnrollmentID, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE enrollments.enrollments SET status = 'Enrolled', pass_number = $2, updated_at = now() WHERE id = $1 AND status = 'Held'`, m.EnrollmentID, m.Number)
		return tag.RowsAffected(), err
	})
}

// MarkRejected: the pass was refused (poison).
func (s *Service) MarkRejected(ctx context.Context, messageID uuid.UUID, m messaging.PassRejected) (MarkOutcome, error) {
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE enrollments.enrollments SET status = 'Rejected', reason = $2, updated_at = now() WHERE id = $1 AND status IN ('Held','Requested')`, m.EnrollmentID, m.Reason)
		return tag.RowsAffected(), err
	})
}

// MarkReleased records that the seats came back.
func (s *Service) MarkReleased(ctx context.Context, messageID uuid.UUID, m messaging.SeatsReleased) (MarkOutcome, error) {
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE enrollments.enrollments SET seats_released_at = now() WHERE id = $1 AND seats_released_at IS NULL`, m.EnrollmentID)
		return tag.RowsAffected(), err
	})
}

var _ = errors.New
