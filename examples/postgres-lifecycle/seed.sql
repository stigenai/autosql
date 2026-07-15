INSERT INTO app.customers (id, email, name)
VALUES (1, 'ada@example.com', 'Ada Lovelace');

INSERT INTO app.orders (id, customer_id, total_cents, status)
VALUES (1001, 1, 4200, 'pending');
