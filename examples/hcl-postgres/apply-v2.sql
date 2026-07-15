ALTER TABLE hcl_demo.accounts
    ADD COLUMN display_name text NOT NULL DEFAULT 'anonymous',
    ADD COLUMN last_seen_at timestamptz;
