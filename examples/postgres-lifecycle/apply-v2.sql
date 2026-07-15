ALTER TABLE app.customers
    ADD COLUMN marketing_opt_in boolean NOT NULL DEFAULT false;

ALTER TABLE app.orders
    ADD COLUMN fulfilled_at timestamptz;
