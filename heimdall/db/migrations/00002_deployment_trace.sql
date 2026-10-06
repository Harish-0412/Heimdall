-- +goose Up
ALTER TABLE heimdall.pull_requests ADD COLUMN trace_parent text NOT NULL DEFAULT ''
  CHECK(length(trace_parent)<=128 AND (trace_parent='' OR trace_parent ~ '^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}(-[0-9a-f]+)*$'));

-- +goose Down
ALTER TABLE heimdall.pull_requests DROP COLUMN trace_parent;
