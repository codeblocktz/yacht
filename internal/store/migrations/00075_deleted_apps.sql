-- What an app was, for a while after it is deleted: a meter reading an hour
-- that ended after the delete still has the traffic the app sent in it, and
-- that traffic is counted under the app's namespace, which nothing else then
-- names. Every pull request's environment is deleted when the pull request
-- closes, so without this its last part-hour is nobody's.

-- +goose Up
-- +goose StatementBegin

-- No reference to teams or projects: the record outlives both, and is read
-- for the hour it describes, not for what is there now.
CREATE TABLE deleted_apps (
    id uuid PRIMARY KEY,
    owner_id text NOT NULL,
    project_id uuid,
    name text NOT NULL,
    namespace text NOT NULL,
    deleted_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX deleted_apps_deleted_at ON deleted_apps (deleted_at);

-- A trigger rather than a statement beside each delete, so that every way an
-- app goes — deleted by itself, with its team, by a migration — is recorded,
-- and in the transaction that deletes it. It also drops what is older than a
-- week, longer than any meter catches up for, so the table stays small with
-- nothing else to run.
CREATE FUNCTION record_deleted_app() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO deleted_apps (id, owner_id, project_id, name, namespace)
    VALUES (OLD.id, OLD.owner_id, OLD.project_id, OLD.name, OLD.namespace)
    ON CONFLICT (id) DO NOTHING;
    DELETE FROM deleted_apps WHERE deleted_at < now() - interval '7 days';
    RETURN OLD;
END;
$$;

CREATE TRIGGER apps_record_deleted
AFTER DELETE ON apps
FOR EACH ROW EXECUTE FUNCTION record_deleted_app();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS apps_record_deleted ON apps;
DROP FUNCTION IF EXISTS record_deleted_app();
DROP TABLE IF EXISTS deleted_apps;
-- +goose StatementEnd
