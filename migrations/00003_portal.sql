-- +goose Up
-- Which link an applicant used to start, so invitations resume the same submission.
ALTER TABLE submissions
    ADD COLUMN form_link_id uuid REFERENCES form_links(id) ON DELETE SET NULL;
CREATE INDEX submissions_link_idx ON submissions (form_link_id);

CREATE INDEX submission_files_field_idx ON submission_files (submission_id, field_key);

-- +goose Down
DROP INDEX submission_files_field_idx;
ALTER TABLE submissions DROP COLUMN form_link_id;
