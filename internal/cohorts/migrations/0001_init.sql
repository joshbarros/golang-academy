CREATE TABLE cohorts.cohorts (
    id          uuid PRIMARY KEY,
    code        text        NOT NULL UNIQUE CHECK (char_length(code) <= 20),
    title     text        NOT NULL,
    track     text        NOT NULL,
    instructor     text        NOT NULL,
    starts_at     timestamptz NOT NULL,
    seat_capacity integer     NOT NULL,
    seats_left  integer     NOT NULL,
    created_at  timestamptz NOT NULL,
    CONSTRAINT cohorts_remaining CHECK (seats_left >= 0 AND seats_left <= seat_capacity)
);

CREATE TABLE cohorts.seat_holds (
    enrollment_id       uuid PRIMARY KEY,
    owner_id       uuid        NOT NULL,
    cohort_id        uuid        NOT NULL REFERENCES cohorts.cohorts (id),
    seats          integer     NOT NULL,
    status         text        NOT NULL,
    created_at     timestamptz NOT NULL,
    released_at    timestamptz,
    release_reason text
);

CREATE TABLE cohorts.outbox_messages (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id  uuid        NOT NULL,
    module      text        NOT NULL,
    routing_key text        NOT NULL,
    key         text        NOT NULL,
    payload     jsonb       NOT NULL,
    created_at  timestamptz NOT NULL,
    sent_at     timestamptz
);
CREATE INDEX cohorts_outbox_unsent ON cohorts.outbox_messages (id) WHERE sent_at IS NULL;

CREATE TABLE cohorts.inbox_messages (
    consumer   text        NOT NULL,
    message_id uuid        NOT NULL,
    claimed_at timestamptz NOT NULL,
    PRIMARY KEY (consumer, message_id)
);
