-- +goose Up
-- Multi-level approvals: each form can define ordered approval steps
-- (e.g. Comercial -> Cumplimiento). A submission is approved only after every
-- step signs off; submissions.approval_step is the index of the pending step.
ALTER TABLE forms ADD COLUMN approval_steps jsonb NOT NULL DEFAULT '[]';
ALTER TABLE submissions ADD COLUMN approval_step int NOT NULL DEFAULT 0;

-- One row per step sign-off. Requesting changes or reopening a decision
-- invalidates earlier sign-offs (the data may change), keeping them for history.
CREATE TABLE submission_approvals (
    id             bigserial PRIMARY KEY,
    submission_id  uuid NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    step           int NOT NULL,
    step_name      text NOT NULL,
    user_id        uuid NOT NULL REFERENCES users(id),
    comment        text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    invalidated_at timestamptz
);
CREATE INDEX submission_approvals_sub_idx ON submission_approvals (submission_id, created_at);

-- +goose Down
DROP TABLE submission_approvals;
ALTER TABLE submissions DROP COLUMN approval_step;
ALTER TABLE forms DROP COLUMN approval_steps;
