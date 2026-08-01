-- +goose Up
-- +goose StatementBegin

-- One app referring to another, discovered from a variable's value.
--
-- Recorded when the variable is written, because that is the only moment the
-- value is readable: a secret is sealed at rest and the page drawing the graph
-- cannot open it. Deriving the edge at read time would mean either decrypting
-- to draw a picture, or drawing a picture that quietly omits every connection
-- somebody was careful about.
CREATE TABLE app_links (
    owner_id   text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    from_app_id uuid NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    to_app_id   uuid NOT NULL REFERENCES apps (id) ON DELETE CASCADE,

    -- The variable that carries the reference, so the edge can say why it is
    -- there rather than asserting a relationship with no evidence.
    via_key text NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (from_app_id, to_app_id, via_key),

    -- An app referring to itself is not a relationship. A database's own
    -- connection string names its own service, and drawing that as an edge
    -- would put a loop on every database.
    CONSTRAINT app_links_not_self CHECK (from_app_id <> to_app_id)
);

CREATE INDEX app_links_owner_id_idx ON app_links (owner_id);
CREATE INDEX app_links_to_app_id_idx ON app_links (to_app_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS app_links;
-- +goose StatementEnd
