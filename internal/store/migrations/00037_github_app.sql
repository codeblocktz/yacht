-- The install's GitHub App: which of its installations each team may build
-- from, which repository an app is connected to through one, and the webhook
-- deliveries already acted on.
--
-- Nothing secret is stored. An installation token is minted when a build or a
-- picker needs one and kept in memory until shortly before it expires; the
-- person's own user token proves their access once, at connect, and is never
-- written down. What is here is ids GitHub hands out and the names that go
-- with them.

-- +goose Up
-- +goose StatementBegin

-- One installation of the App, on a GitHub account or organisation. Its own
-- table rather than columns on the team's connection, because several teams
-- may connect the same installation and GitHub's events about it — suspended,
-- uninstalled — are about it rather than about any one of them.
CREATE TABLE github_installations (
    -- GitHub's own id. Never reused: an App removed and installed again on the
    -- same account is a new installation with a new id.
    id            bigint PRIMARY KEY,
    account_login text   NOT NULL DEFAULT '',
    -- "User" or "Organization", as GitHub spells them.
    account_type  text   NOT NULL DEFAULT '',

    -- Set from the installation webhook. A suspended installation mints no
    -- tokens until it is unsuspended; a removed one never will again.
    suspended_at  timestamptz,
    removed_at    timestamptz,

    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- A team's use of an installation, written only once somebody signed in to
-- GitHub and GitHub listed the installation among theirs. An installation id
-- in a query string proves nothing — anyone can type one — so this row is the
-- proof, and every read of an installation's repositories goes through it.
CREATE TABLE team_github_installations (
    owner_id           text   NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    installation_id    bigint NOT NULL REFERENCES github_installations (id) ON DELETE CASCADE,

    -- Who connected it: the person signed in here, and the GitHub account
    -- that proved access. Text rather than a reference to users, so the
    -- record of who did it outlives their account.
    connected_by_email text   NOT NULL DEFAULT '',
    connected_by_login text   NOT NULL DEFAULT '',
    connected_at       timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (owner_id, installation_id)
);

CREATE INDEX team_github_installations_installation_idx
    ON team_github_installations (installation_id);

-- The repository an app builds from, by the ids GitHub keeps through a
-- rename: a push is routed by the repository's id, never by its name. Null on
-- an app whose repository was pasted as a URL, which is cloned anonymously as
-- before.
--
-- github_paused says why builds from it have stopped, and is empty while they
-- have not: 'repo_removed' when the installation lost the repository,
-- 'installation_suspended' and 'installation_removed' for the installation
-- itself, 'disconnected' when the app's team let go of the installation. A
-- paused app is not deleted and keeps running on its last image;
-- nothing new is built until access comes back or another repository is
-- chosen.
ALTER TABLE apps
    ADD COLUMN github_installation_id bigint,
    ADD COLUMN github_repo_id         bigint,
    ADD COLUMN github_paused          text NOT NULL DEFAULT ''
        CHECK (github_paused IN ('', 'repo_removed', 'installation_suspended',
                                 'installation_removed', 'disconnected')),
    ADD CONSTRAINT apps_github_source_whole
        CHECK ((github_installation_id IS NULL) = (github_repo_id IS NULL));

CREATE INDEX apps_github_repo_idx ON apps (github_repo_id)
    WHERE github_repo_id IS NOT NULL;
CREATE INDEX apps_github_installation_idx ON apps (github_installation_id)
    WHERE github_installation_id IS NOT NULL;

-- GitHub's X-GitHub-Delivery of every delivery acted on, so a redelivery —
-- GitHub's, or somebody replaying one they captured — is answered without
-- deploying again. Kept for a week, which is longer than GitHub retries.
CREATE TABLE github_deliveries (
    id          text PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 100),
    event       text NOT NULL DEFAULT '',
    received_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX github_deliveries_received_idx ON github_deliveries (received_at);

-- A push arriving through the App waits behind a deploy in flight in the same
-- slot a push to the app's own webhook does, and its result is shown in the
-- same place. Such an app need not have a webhook of its own, so the row may
-- now exist without a secret: a null secret is no webhook, and its URL is
-- refused like one that was never made.
ALTER TABLE app_hooks ALTER COLUMN secret DROP NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DELETE FROM app_hooks WHERE secret IS NULL;
ALTER TABLE app_hooks ALTER COLUMN secret SET NOT NULL;
DROP TABLE IF EXISTS github_deliveries;
DROP INDEX IF EXISTS apps_github_installation_idx;
DROP INDEX IF EXISTS apps_github_repo_idx;
ALTER TABLE apps
    DROP CONSTRAINT IF EXISTS apps_github_source_whole,
    DROP COLUMN IF EXISTS github_paused,
    DROP COLUMN IF EXISTS github_repo_id,
    DROP COLUMN IF EXISTS github_installation_id;
DROP TABLE IF EXISTS team_github_installations;
DROP TABLE IF EXISTS github_installations;

-- +goose StatementEnd
