DROP SCHEMA IF EXISTS hcl_advanced CASCADE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'hcl_advanced_app') THEN
        DROP OWNED BY hcl_advanced_app;
        DROP ROLE hcl_advanced_app;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'hcl_advanced_reader') THEN
        DROP OWNED BY hcl_advanced_reader;
        DROP ROLE hcl_advanced_reader;
    END IF;
END
$$;

CREATE ROLE hcl_advanced_reader NOLOGIN;
CREATE ROLE hcl_advanced_app NOLOGIN;
GRANT hcl_advanced_reader TO hcl_advanced_app;

CREATE SCHEMA hcl_advanced;
CREATE EXTENSION "uuid-ossp" WITH SCHEMA hcl_advanced;

CREATE TYPE hcl_advanced.account_status AS ENUM ('pending', 'active', 'disabled');
CREATE DOMAIN hcl_advanced.positive_amount AS numeric(12, 2)
    CHECK (VALUE >= 0);
CREATE TYPE hcl_advanced.contact_info AS (
    email text,
    phone text
);

CREATE SEQUENCE hcl_advanced.account_id_seq
    AS bigint START WITH 1000 INCREMENT BY 1 CACHE 10;

CREATE TABLE hcl_advanced.organizations (
    id bigint NOT NULL,
    name text NOT NULL,
    CONSTRAINT organizations_pkey PRIMARY KEY (id),
    CONSTRAINT organizations_name_key UNIQUE (name)
);

CREATE TABLE hcl_advanced.accounts (
    id bigint NOT NULL DEFAULT nextval('hcl_advanced.account_id_seq'),
    organization_id bigint NOT NULL,
    email text NOT NULL,
    status hcl_advanced.account_status NOT NULL DEFAULT 'pending',
    credit hcl_advanced.positive_amount NOT NULL DEFAULT 0,
    contact hcl_advanced.contact_info,
    tags text[] NOT NULL DEFAULT '{}',
    metadata jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT accounts_pkey PRIMARY KEY (id),
    CONSTRAINT accounts_email_key UNIQUE (email),
    CONSTRAINT accounts_email_check CHECK (position('@' in email) > 1),
    CONSTRAINT accounts_organization_fkey FOREIGN KEY (organization_id)
        REFERENCES hcl_advanced.organizations(id) ON DELETE CASCADE
);

ALTER SEQUENCE hcl_advanced.account_id_seq
    OWNED BY hcl_advanced.accounts.id;

CREATE INDEX accounts_active_org_idx
    ON hcl_advanced.accounts USING btree (organization_id, created_at DESC)
    WHERE status = 'active';

CREATE VIEW hcl_advanced.active_accounts AS
SELECT id, organization_id, email, created_at
FROM hcl_advanced.accounts
WHERE status = 'active';

CREATE MATERIALIZED VIEW hcl_advanced.organization_account_counts AS
SELECT organization_id, count(*) AS account_count
FROM hcl_advanced.accounts
GROUP BY organization_id
WITH NO DATA;

CREATE FUNCTION hcl_advanced.normalize_email(value text)
RETURNS text
LANGUAGE sql
IMMUTABLE
RETURN lower(trim(value));

CREATE FUNCTION hcl_advanced.set_normalized_email()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.email := hcl_advanced.normalize_email(NEW.email);
    RETURN NEW;
END
$$;

CREATE PROCEDURE hcl_advanced.activate_account(account_id bigint)
LANGUAGE sql
AS $$
UPDATE hcl_advanced.accounts SET status = 'active' WHERE id = account_id;
$$;

CREATE TRIGGER accounts_normalize_email
BEFORE INSERT OR UPDATE OF email ON hcl_advanced.accounts
FOR EACH ROW EXECUTE FUNCTION hcl_advanced.set_normalized_email();

ALTER TABLE hcl_advanced.accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE hcl_advanced.accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY accounts_reader_policy ON hcl_advanced.accounts
    FOR SELECT TO hcl_advanced_reader
    USING (status = 'active');

GRANT USAGE ON SCHEMA hcl_advanced TO hcl_advanced_reader;
GRANT SELECT ON hcl_advanced.active_accounts TO hcl_advanced_reader;
ALTER DEFAULT PRIVILEGES IN SCHEMA hcl_advanced
    GRANT SELECT ON TABLES TO hcl_advanced_reader;
