-- +goose Up
CREATE TYPE member_role AS ENUM ('owner', 'admin', 'reviewer');
CREATE TYPE form_status AS ENUM ('draft', 'published', 'archived');
CREATE TYPE link_kind AS ENUM ('public', 'invite');
CREATE TYPE submission_status AS ENUM (
    'draft', 'submitted', 'in_review', 'changes_requested', 'approved', 'rejected'
);
CREATE TYPE actor_type AS ENUM ('user', 'applicant', 'system');

CREATE TABLE organizations (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL,
    slug          text NOT NULL UNIQUE,
    branding      jsonb NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text NOT NULL UNIQUE,
    name          text NOT NULL DEFAULT '',
    password_hash text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    role            member_role NOT NULL,
    PRIMARY KEY (user_id, organization_id)
);

CREATE TABLE sessions (
    id          text PRIMARY KEY, -- sha256 of the cookie token
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user_idx ON sessions (user_id);

CREATE TABLE forms (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id    uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    title              text NOT NULL,
    description        text NOT NULL DEFAULT '',
    status             form_status NOT NULL DEFAULT 'draft',
    draft_schema       jsonb NOT NULL DEFAULT '{"sections":[]}',
    current_version_id uuid,
    created_by         uuid REFERENCES users(id),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX forms_org_idx ON forms (organization_id, status);

-- Published versions are immutable: a submission always renders with the schema it was filled on.
CREATE TABLE form_versions (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    form_id        uuid NOT NULL REFERENCES forms(id) ON DELETE CASCADE,
    version_number int  NOT NULL,
    schema         jsonb NOT NULL,
    published_at   timestamptz NOT NULL DEFAULT now(),
    published_by   uuid REFERENCES users(id),
    UNIQUE (form_id, version_number)
);

ALTER TABLE forms
    ADD CONSTRAINT forms_current_version_fk
    FOREIGN KEY (current_version_id) REFERENCES form_versions(id);

CREATE TABLE form_links (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    form_id        uuid NOT NULL REFERENCES forms(id) ON DELETE CASCADE,
    token          text NOT NULL UNIQUE,
    kind           link_kind NOT NULL DEFAULT 'public',
    invitee_email  text,
    expires_at     timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE submissions (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id   uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    form_id           uuid NOT NULL REFERENCES forms(id),
    form_version_id   uuid NOT NULL REFERENCES form_versions(id),
    applicant_email   text NOT NULL,
    applicant_name    text NOT NULL DEFAULT '',
    access_token_hash text NOT NULL UNIQUE,
    status            submission_status NOT NULL DEFAULT 'draft',
    data              jsonb NOT NULL DEFAULT '{}',
    assigned_to       uuid REFERENCES users(id),
    submitted_at      timestamptz,
    decided_at        timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX submissions_inbox_idx ON submissions (organization_id, status, submitted_at DESC);
CREATE INDEX submissions_form_idx  ON submissions (form_id, created_at DESC);
CREATE INDEX submissions_email_idx ON submissions (lower(applicant_email));
CREATE INDEX submissions_data_idx  ON submissions USING gin (data jsonb_path_ops);

CREATE TABLE submission_files (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    submission_id uuid NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    field_key     text NOT NULL,
    storage_key   text NOT NULL,
    filename      text NOT NULL,
    mime_type     text NOT NULL,
    size_bytes    bigint NOT NULL,
    uploaded_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX submission_files_sub_idx ON submission_files (submission_id);

CREATE TABLE review_comments (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    submission_id  uuid NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    author_user_id uuid NOT NULL REFERENCES users(id),
    field_key      text, -- null = comment on the whole submission
    body           text NOT NULL,
    resolved_at    timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX review_comments_sub_idx ON review_comments (submission_id, created_at);

-- Append-only trail of everything that happens to a submission.
CREATE TABLE audit_events (
    id            bigserial PRIMARY KEY,
    submission_id uuid REFERENCES submissions(id) ON DELETE CASCADE,
    actor_type    actor_type NOT NULL,
    actor_id      text,
    action        text NOT NULL,
    from_status   submission_status,
    to_status     submission_status,
    metadata      jsonb NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_events_sub_idx ON audit_events (submission_id, created_at);

-- +goose Down
DROP TABLE audit_events, review_comments, submission_files, submissions, form_links;
ALTER TABLE forms DROP CONSTRAINT forms_current_version_fk;
DROP TABLE form_versions, forms, sessions, memberships, users, organizations;
DROP TYPE actor_type, submission_status, link_kind, form_status, member_role;
