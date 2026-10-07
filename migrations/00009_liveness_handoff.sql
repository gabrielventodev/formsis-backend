-- +goose Up
-- Liveness from a phone: on a computer the portal shows a QR with a short-lived link that lets
-- a phone take the liveness check of one field (and nothing else) while the computer waits.
CREATE TABLE liveness_handoffs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    submission_id uuid NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    field_key     text NOT NULL,
    -- sha256 of the token in the QR; the token itself is never stored.
    token_hash    text NOT NULL UNIQUE,
    expires_at    timestamptz NOT NULL,
    -- When the phone first opened the link.
    opened_at     timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX liveness_handoffs_submission_idx ON liveness_handoffs (submission_id, field_key);

-- The handoff an attempt was taken through (NULL = taken on the same device).
ALTER TABLE liveness_checks ADD COLUMN handoff_id uuid REFERENCES liveness_handoffs(id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE liveness_checks DROP COLUMN handoff_id;
DROP TABLE liveness_handoffs;
