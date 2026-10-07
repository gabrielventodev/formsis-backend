-- +goose Up
-- Liveness checks: the applicant follows a random challenge in front of the camera
-- (look ahead, turn, come closer) and the face service decides whether it was a live
-- person. Every attempt is kept with its frames so reviewers can look at them.
CREATE TABLE liveness_checks (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    submission_id uuid NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    field_key     text NOT NULL,
    steps         text[] NOT NULL,
    -- NULL while the challenge is waiting for frames.
    decision      text CHECK (decision IN ('pass', 'review', 'retry', 'fail', 'expired', 'error')),
    reasons       text[] NOT NULL DEFAULT '{}',
    -- Full answer of the face service: per-frame measurements, scores, model versions.
    result        jsonb,
    -- Stored frames, in capture order: [{"step": 0, "key": "..."}].
    frames        jsonb NOT NULL DEFAULT '[]',
    best_frame    int,
    expires_at    timestamptz NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    completed_at  timestamptz
);
CREATE INDEX liveness_checks_submission_idx ON liveness_checks (submission_id, field_key, created_at);

-- +goose Down
DROP TABLE liveness_checks;
