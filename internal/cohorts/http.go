package cohorts

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-academy/internal/platform/httpx"
)

const maxSeatCapacity = 500

// Cohort is the public view of a cohort.
type Cohort struct {
	ID           uuid.UUID `json:"id"`
	Code         string    `json:"code"`
	Title        string    `json:"title"`
	Track        string    `json:"track"`
	Instructor   string    `json:"instructor"`
	StartsAt     time.Time `json:"startsAt"`
	SeatCapacity int       `json:"seatCapacity"`
	SeatsLeft    int       `json:"seatsLeft"`
}

// Info is what other modules may ask the cohorts module (Enrollments never reads this schema).
type Info struct {
	ID   uuid.UUID
	Code string
}

// Catalog answers the one cross-module read: a cohort's existence.
type Catalog struct{ Pool *pgxpool.Pool }

// Find looks a cohort up by code. It returns nil when there is none.
func (c Catalog) Find(ctx context.Context, code string) (*Info, error) {
	var i Info
	err := c.Pool.QueryRow(ctx, `SELECT id, code FROM cohorts.cohorts WHERE code = $1`, code).Scan(&i.ID, &i.Code)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &i, err
}

// API is the cohorts HTTP surface.
type API struct {
	Pool *pgxpool.Pool
	Auth httpx.Authenticator
	Log  *slog.Logger
}

// Routes registers the cohort endpoints; every one needs a signed-in user.
func (a *API) Routes(mux *http.ServeMux) {
	authed := func(h httpx.HandlerFunc) http.HandlerFunc {
		return httpx.Authenticated(a.Log, a.Auth, httpx.Wrap(a.Log, h))
	}
	mux.HandleFunc("GET /api/cohorts", authed(a.list))
	mux.HandleFunc("GET /api/cohorts/{code}", authed(a.get))
	mux.HandleFunc("POST /api/cohorts", httpx.Authenticated(a.Log, a.Auth, httpx.Staff(httpx.Wrap(a.Log, a.create))))
}

const cohortSelect = `SELECT id, code, title, track, instructor, starts_at, seat_capacity, seats_left FROM cohorts.cohorts`

func scanCohort(row pgx.Row) (Cohort, error) {
	var d Cohort
	err := row.Scan(&d.ID, &d.Code, &d.Title, &d.Track, &d.Instructor, &d.StartsAt, &d.SeatCapacity, &d.SeatsLeft)
	return d, err
}

func (a *API) list(w http.ResponseWriter, r *http.Request) error {
	rows, err := a.Pool.Query(r.Context(), cohortSelect+` ORDER BY starts_at, code`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []Cohort{}
	for rows.Next() {
		d, err := scanCohort(rows)
		if err != nil {
			return err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, out)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) error {
	d, err := scanCohort(a.Pool.QueryRow(r.Context(), cohortSelect+` WHERE code = $1`, r.PathValue("code")))
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.Fail(http.StatusNotFound, "Not found.")
	}
	if err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, d)
}

type createRequest struct {
	Code         string     `json:"code"`
	Title        string     `json:"title"`
	Track        string     `json:"track"`
	Instructor   string     `json:"instructor"`
	StartsAt     *time.Time `json:"startsAt"`
	SeatCapacity int64      `json:"seatCapacity"`
}

// create opens a cohort. Staff only. This is the only place seats_left is initialised; after that only the reserve and release
// handlers write it.
func (a *API) create(w http.ResponseWriter, r *http.Request) error {
	var req createRequest
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	code := strings.TrimSpace(req.Code)
	if code == "" || len(code) > 20 {
		return httpx.Fail(http.StatusBadRequest, "Code is required and at most 20 characters.")
	}
	if strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.Track) == "" || strings.TrimSpace(req.Instructor) == "" {
		return httpx.Fail(http.StatusBadRequest, "Title, track and instructor are required.")
	}
	if req.SeatCapacity < 1 || req.SeatCapacity > maxSeatCapacity {
		return httpx.Fail(http.StatusBadRequest, "Seat capacity must be between 1 and %d seats.", maxSeatCapacity)
	}
	when := time.Now().AddDate(0, 0, 14)
	if req.StartsAt != nil {
		when = *req.StartsAt
	}
	id, _ := uuid.NewV7()
	_, err := a.Pool.Exec(r.Context(), `
		INSERT INTO cohorts.cohorts (id, code, title, track, instructor, starts_at, seat_capacity, seats_left, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7,now())`,
		id, code, strings.TrimSpace(req.Title), strings.TrimSpace(req.Track), strings.TrimSpace(req.Instructor), when, req.SeatCapacity)
	if err != nil {
		var coder interface{ SQLState() string }
		if errors.As(err, &coder) && coder.SQLState() == "23505" {
			return httpx.Fail(http.StatusConflict, "Cohort '%s' already exists.", code)
		}
		return err
	}
	d, err := scanCohort(a.Pool.QueryRow(r.Context(), cohortSelect+` WHERE id = $1`, id))
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/cohorts/"+code)
	return httpx.JSON(w, http.StatusCreated, d)
}
