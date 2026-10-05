-- +goose Up
-- The MVP runs for a single organization; the data model already supports several.
INSERT INTO organizations (name, slug)
SELECT 'Mi organización', 'default'
WHERE NOT EXISTS (SELECT 1 FROM organizations);

-- +goose Down
SELECT 1;
