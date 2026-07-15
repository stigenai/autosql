CREATE SCHEMA IF NOT EXISTS app;

CREATE TABLE IF NOT EXISTS app.customers (
    id bigint PRIMARY KEY,
    email text NOT NULL UNIQUE,
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS app.orders (
    id bigint PRIMARY KEY,
    customer_id bigint NOT NULL REFERENCES app.customers(id),
    total_cents integer NOT NULL CHECK (total_cents >= 0),
    status text NOT NULL DEFAULT 'pending',
    created_at timestamptz NOT NULL DEFAULT now()
);
