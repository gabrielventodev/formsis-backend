-- +goose Up
-- Outgoing webhooks: an organization registers HTTPS endpoints that receive a
-- signed POST when a submission changes status. Deliveries are queued in the
-- same transaction as the change and sent (with retries) by a background worker.
CREATE TABLE webhooks (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    url             text NOT NULL,
    description     text NOT NULL DEFAULT '',
    secret          text NOT NULL, -- HMAC key; must be readable to sign
    events          text[] NOT NULL,
    include_data    boolean NOT NULL DEFAULT false,
    active          boolean NOT NULL DEFAULT true,
    created_by      uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhooks_org_idx ON webhooks (organization_id);

CREATE TABLE webhook_deliveries (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    webhook_id       uuid NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    event            text NOT NULL,
    payload          jsonb NOT NULL,
    status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'succeeded', 'failed')),
    attempts         int NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    last_status_code int,
    last_error       text NOT NULL DEFAULT '',
    last_attempt_at  timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX webhook_deliveries_hook_idx ON webhook_deliveries (webhook_id, created_at DESC);

-- +goose Down
DROP TABLE webhook_deliveries;
DROP TABLE webhooks;
