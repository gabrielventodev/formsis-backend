-- +goose Up
-- Inbox filters for the admin panel: "assigned to me" and free-text search by applicant.
CREATE INDEX submissions_assigned_idx ON submissions (organization_id, assigned_to, submitted_at DESC)
    WHERE assigned_to IS NOT NULL;
CREATE INDEX submissions_name_idx ON submissions (lower(applicant_name));
CREATE INDEX audit_events_created_idx ON audit_events (created_at DESC);

-- +goose Down
DROP INDEX audit_events_created_idx, submissions_name_idx, submissions_assigned_idx;
