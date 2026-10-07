-- +goose Up
-- Team management: invited reviewers set their own password through an
-- emailed link, owners and admins can deactivate members, and team changes
-- land in the audit trail (with no submission, so they carry the org).
ALTER TABLE users ADD COLUMN last_login_at timestamptz;
ALTER TABLE memberships
    ADD COLUMN disabled_at timestamptz,
    ADD COLUMN created_at  timestamptz NOT NULL DEFAULT now();

CREATE TABLE password_tokens (
    id         text PRIMARY KEY, -- sha256 of the emailed token
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    purpose    text NOT NULL CHECK (purpose IN ('invite', 'reset')),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX password_tokens_user_idx ON password_tokens (user_id);

ALTER TABLE audit_events ADD COLUMN organization_id uuid REFERENCES organizations(id) ON DELETE CASCADE;
CREATE INDEX audit_events_org_idx ON audit_events (organization_id, created_at DESC) WHERE organization_id IS NOT NULL;

-- +goose Down
DROP INDEX audit_events_org_idx;
ALTER TABLE audit_events DROP COLUMN organization_id;
DROP TABLE password_tokens;
ALTER TABLE memberships DROP COLUMN created_at, DROP COLUMN disabled_at;
ALTER TABLE users DROP COLUMN last_login_at;
