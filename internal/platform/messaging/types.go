// Package messaging is the transactional outbox, the inbox, and the RabbitMQ and Kafka plumbing around them.
package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Names of the exchanges, the Kafka topic and the routing keys. They are the contract between the modules.
const (
	Exchange         = "academy.work"
	DeadLetterExch   = "academy.dlx"
	RequeueExchange  = "academy.requeue"
	KafkaTopic       = "academy.events"
	RetryDelayMs     = 2000
	MaxAttempts      = 3
	ReserveKey       = "seats.reserve"
	ReleaseKey       = "seats.release"
	HeldKey          = "seats.held"
	ShortKey         = "seats.short"
	ReleasedKey      = "seats.released"
	IssuedKey        = "pass.issued"
	RejectedKey      = "pass.rejected"
	CohortsQueue     = "cohorts.commands"
	PassesQueue      = "passes.work"
	EnrollmentsQueue = "enrollments.events"
	PoisonToken      = "POISON"
	attemptHeader    = "x-attempt"
	outboxBatchSize  = 50
	relayIdleDelay   = 200 * time.Millisecond
	relayErrorDelay  = time.Second
	trimEvery        = 10 * time.Minute
)

// Delivery is one message handed to a Handler.
type Delivery struct {
	MessageID  uuid.UUID
	RoutingKey string
	Body       []byte
}

// Handler consumes one message. Returning ErrPoison dead-letters it at once; any other error retries it.
type Handler interface {
	Handle(ctx context.Context, d Delivery) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, d Delivery) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, d Delivery) error { return f(ctx, d) }

// ErrPoison marks a message whose rejection is already committed: it must be dead-lettered, not retried.
var ErrPoison = errors.New("poison message")

// OutOfEnrollmentError means the message arrived before the state it depends on; it is retried later.
type OutOfEnrollmentError struct{ Reason string }

func (e *OutOfEnrollmentError) Error() string { return e.Reason }

// IsPoison reports whether the reference carries the poison token (a case-sensitive substring).
func IsPoison(reference string) bool { return strings.Contains(reference, PoisonToken) }

// FailureAction is what a consumer does with a message whose handler failed.
type FailureAction int

// The two things a failed message can meet.
const (
	ActionRetry FailureAction = iota
	ActionDeadLetter
)

// Decide: a poison message is dead-lettered at once; anything else is retried until it has been tried MaxAttempts times.
func Decide(priorAttempts int, err error) FailureAction {
	if errors.Is(err, ErrPoison) {
		return ActionDeadLetter
	}
	if priorAttempts+1 >= MaxAttempts {
		return ActionDeadLetter
	}
	return ActionRetry
}

// ---- the messages ----

// ReserveSeats asks the cohort to hold seats.
type ReserveSeats struct {
	EnrollmentID uuid.UUID `json:"enrollmentId"`
	OwnerID      uuid.UUID `json:"ownerId"`
	CohortID     uuid.UUID `json:"cohortId"`
	Seats        int       `json:"seats"`
	Reference    string    `json:"reference"`
}

// SeatsHeld says the seats are held.
type SeatsHeld struct {
	EnrollmentID uuid.UUID `json:"enrollmentId"`
	OwnerID      uuid.UUID `json:"ownerId"`
	CohortID     uuid.UUID `json:"cohortId"`
	Seats        int       `json:"seats"`
	Reference    string    `json:"reference"`
}

// SeatsShort says the reserve was refused: the remaining seats did not cover it.
type SeatsShort struct {
	EnrollmentID   uuid.UUID `json:"enrollmentId"`
	Reference      string    `json:"reference"`
	RequestedSeats int       `json:"requestedSeats"`
	SeatsLeft      int       `json:"seatsLeft"`
}

// ReleaseSeats asks the cohort to give held seats back.
type ReleaseSeats struct {
	EnrollmentID uuid.UUID `json:"enrollmentId"`
	Reason       string    `json:"reason"`
}

// SeatsReleased says seats (possibly none) were given back.
type SeatsReleased struct {
	EnrollmentID uuid.UUID `json:"enrollmentId"`
	Seats        int       `json:"seats"`
	Reason       string    `json:"reason"`
}

// PassIssued says the pass exists.
type PassIssued struct {
	EnrollmentID uuid.UUID `json:"enrollmentId"`
	Number       string    `json:"number"`
}

// PassRejected says the pass was refused (the poison rule).
type PassRejected struct {
	EnrollmentID uuid.UUID `json:"enrollmentId"`
	Reason       string    `json:"reason"`
}

// Parse decodes a message body.
func Parse[T any](body []byte) (T, error) {
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return v, err
	}
	return v, nil
}
