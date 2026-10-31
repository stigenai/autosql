CREATE SCHEMA project_demo;
CREATE TABLE project_demo.accounts (
    id bigint NOT NULL,
    label text NOT NULL DEFAULT 'preserved'
);
