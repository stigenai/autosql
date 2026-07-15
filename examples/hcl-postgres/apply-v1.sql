DROP SCHEMA IF EXISTS hcl_demo CASCADE;
CREATE SCHEMA hcl_demo;

CREATE TABLE hcl_demo.accounts (
    id bigint NOT NULL,
    email text NOT NULL
);
