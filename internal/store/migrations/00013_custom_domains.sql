-- +goose Up
-- +goose StatementBegin

-- Custom domains, on the groundwork 00002 already laid.
--
-- That migration split managed from custom and gave each app one managed host
-- by partial unique index. What was missing is proof: the host column is
-- globally unique — DNS has one owner per name — so an unproven claim would let
-- the first team to type "example.com" hold it against every other team on the
-- install, permanently and with no recourse.
--
-- Routing is gated on verification instead. An unverified row is a claim, not a
-- route: nothing reaches the Ingress until the DNS points where it should, and
-- until then somebody who genuinely controls the domain can take it.
ALTER TABLE domains ADD COLUMN verified_at timestamptz;

-- Where the CNAME had to point when the claim was proven.
--
-- Recorded per domain rather than compared against the current platform target
-- at read time, so changing that target does not silently invalidate every
-- domain already verified against the old one. It becomes a visible re-verify.
ALTER TABLE domains ADD COLUMN verify_target text NOT NULL DEFAULT '';

-- Everything already here was issued by the platform, and a platform host needs
-- no proof: the platform owns the wildcard it came from.
UPDATE domains SET verified_at = created_at WHERE managed AND verified;

-- How this install talks to DNS.
--
-- One row, install-wide, and no owner_id: these configure one ExternalDNS
-- controller shared by every team, the same reasoning as cluster_join. A
-- per-app copy would mean whichever app saved last decided for all of them.
CREATE TABLE platform_dns (
    id integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),

    -- What a custom domain's CNAME must point at, and what ExternalDNS is told
    -- to target so it issues a CNAME instead of exposing raw node IPs.
    cname_target text NOT NULL DEFAULT '',

    -- Must match --txt-prefix on the ExternalDNS controller. Yacht does not
    -- deploy that controller and cannot set the flag; this is recorded so the
    -- two can be kept in step, and the page says so rather than implying the
    -- value takes effect from here.
    txt_prefix text NOT NULL DEFAULT 'extdns-',

    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS platform_dns;
ALTER TABLE domains DROP COLUMN IF EXISTS verify_target;
ALTER TABLE domains DROP COLUMN IF EXISTS verified_at;
-- +goose StatementEnd
