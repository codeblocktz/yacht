-- +goose Up
-- Every mail the install tried to send: when, and the error if it failed.
--
-- A send error was only logged, and the dashboard's own log does not outlive
-- its pod, so a relay that started refusing mail was invisible until somebody
-- said their sign-in link never came. Kept here it survives a restart and
-- Settings can say when mail last went out and what the last failure said.
-- No recipient and no body: what is needed is whether mail works, not whom it
-- went to. Rows older than a week are pruned as new ones arrive.
CREATE TABLE mail_deliveries (
    id         bigserial   PRIMARY KEY,
    sent_at    timestamptz NOT NULL DEFAULT now(),
    error      text
);

CREATE INDEX mail_deliveries_sent_at_idx ON mail_deliveries (sent_at);

-- +goose Down
DROP TABLE mail_deliveries;
