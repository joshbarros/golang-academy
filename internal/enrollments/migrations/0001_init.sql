CREATE TABLE enrollments.enrollments (
    id                uuid PRIMARY KEY,
    owner_id          uuid        NOT NULL,
    cohort_id         uuid        NOT NULL,
    cohort_code       text        NOT NULL,
    reference         text        NOT NULL CHECK (char_length(reference) <= 40),
    seats             integer     NOT NULL,
    fee_cents         bigint      NOT NULL,
    status            text        NOT NULL,
    reason            text,
    pass_number      text,
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL,
    seats_released_at timestamptz,
    UNIQUE (cohort_id, reference)
);
CREATE INDEX enrollments_owner_id ON enrollments.enrollments (owner_id);
CREATE INDEX enrollments_created_at ON enrollments.enrollments (created_at DESC);

CREATE TABLE enrollments.outbox_messages (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id  uuid        NOT NULL,
    module      text        NOT NULL,
    routing_key text        NOT NULL,
    key         text        NOT NULL,
    payload     jsonb       NOT NULL,
    created_at  timestamptz NOT NULL,
    sent_at     timestamptz
);
CREATE INDEX enrollments_outbox_unsent ON enrollments.outbox_messages (id) WHERE sent_at IS NULL;

CREATE TABLE enrollments.inbox_messages (
    consumer   text        NOT NULL,
    message_id uuid        NOT NULL,
    claimed_at timestamptz NOT NULL,
    PRIMARY KEY (consumer, message_id)
);

