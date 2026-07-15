resource "check_constraint" "accounts_email_check" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"deferrable\":false,\"definition\":\"CHECK (POSITION(('@'::text) IN (email)) \\u003e 1)\",\"initially_deferred\":false,\"validated\":true}"
  deps_json = "[{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"contains\"}]"
}

resource "column" "metadata" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"default\":\"'{}'::jsonb\",\"not_null\":true,\"ordinal\":8,\"type\":\"jsonb\"}"
  deps_json = "[{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"contains\"}]"
}

resource "column" "email" {
  schema    = "hcl_advanced"
  parent    = "view:04f3d22ad3913bf0d5c22979"
  spec_json = "{\"not_null\":false,\"ordinal\":3,\"type\":\"text\"}"
  deps_json = "[{\"target\":\"view:04f3d22ad3913bf0d5c22979\",\"type\":\"contains\"}]"
}

resource "column" "organization_id" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"not_null\":true,\"ordinal\":2,\"type\":\"bigint\"}"
  deps_json = "[{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"contains\"}]"
}

resource "column" "email" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"not_null\":true,\"ordinal\":3,\"type\":\"text\"}"
  deps_json = "[{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"contains\"}]"
}

resource "column" "account_count" {
  schema    = "hcl_advanced"
  parent    = "materialized_view:ea235edecb7a1a07a02b3bce"
  spec_json = "{\"not_null\":false,\"ordinal\":2,\"type\":\"bigint\"}"
  deps_json = "[{\"target\":\"materialized_view:ea235edecb7a1a07a02b3bce\",\"type\":\"contains\"}]"
}

resource "column" "name" {
  schema    = "hcl_advanced"
  parent    = "table:3e9c73b9fd428a45efaa9c6d"
  spec_json = "{\"not_null\":true,\"ordinal\":2,\"type\":\"text\"}"
  deps_json = "[{\"target\":\"table:3e9c73b9fd428a45efaa9c6d\",\"type\":\"contains\"}]"
}

resource "column" "tags" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"default\":\"'{}'::text[]\",\"not_null\":true,\"ordinal\":7,\"type\":\"text[]\"}"
  deps_json = "[{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"contains\"}]"
}

resource "column" "credit" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"default\":\"0\",\"not_null\":true,\"ordinal\":5,\"type\":\"hcl_advanced.positive_amount\"}"
  deps_json = "[{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"contains\"},{\"target\":\"domain:6e61de9c790cada869ae0800\",\"type\":\"uses\"}]"
}

resource "column" "organization_id" {
  schema    = "hcl_advanced"
  parent    = "view:04f3d22ad3913bf0d5c22979"
  spec_json = "{\"not_null\":false,\"ordinal\":2,\"type\":\"bigint\"}"
  deps_json = "[{\"target\":\"view:04f3d22ad3913bf0d5c22979\",\"type\":\"contains\"}]"
}

resource "column" "created_at" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"default\":\"now()\",\"not_null\":true,\"ordinal\":9,\"type\":\"timestamp with time zone\"}"
  deps_json = "[{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"contains\"}]"
}

resource "column" "created_at" {
  schema    = "hcl_advanced"
  parent    = "view:04f3d22ad3913bf0d5c22979"
  spec_json = "{\"not_null\":false,\"ordinal\":4,\"type\":\"timestamp with time zone\"}"
  deps_json = "[{\"target\":\"view:04f3d22ad3913bf0d5c22979\",\"type\":\"contains\"}]"
}

resource "column" "status" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"default\":\"'pending'::hcl_advanced.account_status\",\"not_null\":true,\"ordinal\":4,\"type\":\"hcl_advanced.account_status\"}"
  deps_json = "[{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"contains\"},{\"target\":\"enum:0754d221db620adc3c60f75b\",\"type\":\"uses\"}]"
}

resource "column" "contact" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"not_null\":false,\"ordinal\":6,\"type\":\"hcl_advanced.contact_info\"}"
  deps_json = "[{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"contains\"},{\"target\":\"composite_type:ae2966747af5f9f0a9af0317\",\"type\":\"uses\"}]"
}

resource "column" "id" {
  schema    = "hcl_advanced"
  parent    = "view:04f3d22ad3913bf0d5c22979"
  spec_json = "{\"not_null\":false,\"ordinal\":1,\"type\":\"bigint\"}"
  deps_json = "[{\"target\":\"view:04f3d22ad3913bf0d5c22979\",\"type\":\"contains\"}]"
}

resource "column" "id" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"default\":\"nextval('hcl_advanced.account_id_seq'::regclass)\",\"not_null\":true,\"ordinal\":1,\"type\":\"bigint\"}"
  deps_json = "[{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"contains\"}]"
}

resource "column" "id" {
  schema    = "hcl_advanced"
  parent    = "table:3e9c73b9fd428a45efaa9c6d"
  spec_json = "{\"not_null\":true,\"ordinal\":1,\"type\":\"bigint\"}"
  deps_json = "[{\"target\":\"table:3e9c73b9fd428a45efaa9c6d\",\"type\":\"contains\"}]"
}

resource "column" "organization_id" {
  schema    = "hcl_advanced"
  parent    = "materialized_view:ea235edecb7a1a07a02b3bce"
  spec_json = "{\"not_null\":false,\"ordinal\":1,\"type\":\"bigint\"}"
  deps_json = "[{\"target\":\"materialized_view:ea235edecb7a1a07a02b3bce\",\"type\":\"contains\"}]"
}

resource "composite_type" "contact_info" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"attributes\":[{\"name\":\"email\",\"not_null\":false,\"type\":\"text\"},{\"name\":\"phone\",\"not_null\":false,\"type\":\"text\"}]}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "default_privilege" "postgres:hcl_advanced:r:hcl_advanced_reader:select" {
  spec_json = "{\"grantable\":false,\"grantee\":\"hcl_advanced_reader\",\"object_type\":\"r\",\"owner\":\"postgres\",\"privilege\":\"SELECT\",\"schema\":\"hcl_advanced\"}"
  deps_json = "[{\"target\":\"role:56e576e5ae5de9009d76c962\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "domain" "positive_amount" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"base_type\":\"numeric(12,2)\",\"constraints\":[\"CHECK (VALUE \\u003e= 0::numeric)\"],\"not_null\":false}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "enum" "account_status" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"values\":[\"pending\",\"active\",\"disabled\"]}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "extension" "uuid-ossp" {
  schema           = "hcl_advanced"
  parent           = "schema:a025e821f1dbbae90052fb5b"
  spec_json        = "{\"version\":\"1.1\"}"
  deps_json        = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
  annotations_json = "{\"comment\":\"generate universally unique identifiers (UUIDs)\"}"
}

resource "foreign_key" "accounts_organization_fkey" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"deferrable\":false,\"definition\":\"FOREIGN KEY (organization_id) REFERENCES hcl_advanced.organizations(id) ON DELETE CASCADE\",\"initially_deferred\":false,\"validated\":true}"
  deps_json = "[{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"contains\"},{\"target\":\"table:3e9c73b9fd428a45efaa9c6d\",\"type\":\"references\"}]"
}

resource "function" "uuid_nil()" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"definition\":\"CREATE OR REPLACE FUNCTION hcl_advanced.uuid_nil()\\n RETURNS uuid\\n LANGUAGE c\\n IMMUTABLE PARALLEL SAFE STRICT\\nAS '$libdir/uuid-ossp', $function$uuid_nil$function$\\n\",\"identity_arguments\":\"\",\"language\":\"c\",\"leakproof\":false,\"name\":\"uuid_nil\",\"parallel\":\"s\",\"result\":\"uuid\",\"security_definer\":false,\"volatility\":\"i\"}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "function" "uuid_generate_v4()" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"definition\":\"CREATE OR REPLACE FUNCTION hcl_advanced.uuid_generate_v4()\\n RETURNS uuid\\n LANGUAGE c\\n PARALLEL SAFE STRICT\\nAS '$libdir/uuid-ossp', $function$uuid_generate_v4$function$\\n\",\"identity_arguments\":\"\",\"language\":\"c\",\"leakproof\":false,\"name\":\"uuid_generate_v4\",\"parallel\":\"s\",\"result\":\"uuid\",\"security_definer\":false,\"volatility\":\"v\"}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "function" "uuid_generate_v1mc()" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"definition\":\"CREATE OR REPLACE FUNCTION hcl_advanced.uuid_generate_v1mc()\\n RETURNS uuid\\n LANGUAGE c\\n PARALLEL SAFE STRICT\\nAS '$libdir/uuid-ossp', $function$uuid_generate_v1mc$function$\\n\",\"identity_arguments\":\"\",\"language\":\"c\",\"leakproof\":false,\"name\":\"uuid_generate_v1mc\",\"parallel\":\"s\",\"result\":\"uuid\",\"security_definer\":false,\"volatility\":\"v\"}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "function" "normalize_email(value text)" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"definition\":\"CREATE OR REPLACE FUNCTION hcl_advanced.normalize_email(value text)\\n RETURNS text\\n LANGUAGE sql\\n IMMUTABLE\\nRETURN lower(TRIM(BOTH FROM value))\\n\",\"identity_arguments\":\"value text\",\"language\":\"sql\",\"leakproof\":false,\"name\":\"normalize_email\",\"parallel\":\"u\",\"result\":\"text\",\"security_definer\":false,\"volatility\":\"i\"}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "function" "uuid_ns_oid()" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"definition\":\"CREATE OR REPLACE FUNCTION hcl_advanced.uuid_ns_oid()\\n RETURNS uuid\\n LANGUAGE c\\n IMMUTABLE PARALLEL SAFE STRICT\\nAS '$libdir/uuid-ossp', $function$uuid_ns_oid$function$\\n\",\"identity_arguments\":\"\",\"language\":\"c\",\"leakproof\":false,\"name\":\"uuid_ns_oid\",\"parallel\":\"s\",\"result\":\"uuid\",\"security_definer\":false,\"volatility\":\"i\"}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "function" "set_normalized_email()" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"definition\":\"CREATE OR REPLACE FUNCTION hcl_advanced.set_normalized_email()\\n RETURNS trigger\\n LANGUAGE plpgsql\\nAS $function$\\nBEGIN\\n    NEW.email := hcl_advanced.normalize_email(NEW.email);\\n    RETURN NEW;\\nEND\\n$function$\\n\",\"identity_arguments\":\"\",\"language\":\"plpgsql\",\"leakproof\":false,\"name\":\"set_normalized_email\",\"parallel\":\"u\",\"result\":\"trigger\",\"security_definer\":false,\"volatility\":\"v\"}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "function" "uuid_ns_x500()" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"definition\":\"CREATE OR REPLACE FUNCTION hcl_advanced.uuid_ns_x500()\\n RETURNS uuid\\n LANGUAGE c\\n IMMUTABLE PARALLEL SAFE STRICT\\nAS '$libdir/uuid-ossp', $function$uuid_ns_x500$function$\\n\",\"identity_arguments\":\"\",\"language\":\"c\",\"leakproof\":false,\"name\":\"uuid_ns_x500\",\"parallel\":\"s\",\"result\":\"uuid\",\"security_definer\":false,\"volatility\":\"i\"}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "function" "uuid_ns_url()" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"definition\":\"CREATE OR REPLACE FUNCTION hcl_advanced.uuid_ns_url()\\n RETURNS uuid\\n LANGUAGE c\\n IMMUTABLE PARALLEL SAFE STRICT\\nAS '$libdir/uuid-ossp', $function$uuid_ns_url$function$\\n\",\"identity_arguments\":\"\",\"language\":\"c\",\"leakproof\":false,\"name\":\"uuid_ns_url\",\"parallel\":\"s\",\"result\":\"uuid\",\"security_definer\":false,\"volatility\":\"i\"}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "function" "uuid_generate_v1()" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"definition\":\"CREATE OR REPLACE FUNCTION hcl_advanced.uuid_generate_v1()\\n RETURNS uuid\\n LANGUAGE c\\n PARALLEL SAFE STRICT\\nAS '$libdir/uuid-ossp', $function$uuid_generate_v1$function$\\n\",\"identity_arguments\":\"\",\"language\":\"c\",\"leakproof\":false,\"name\":\"uuid_generate_v1\",\"parallel\":\"s\",\"result\":\"uuid\",\"security_definer\":false,\"volatility\":\"v\"}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "function" "uuid_generate_v3(namespace uuid, name text)" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"definition\":\"CREATE OR REPLACE FUNCTION hcl_advanced.uuid_generate_v3(namespace uuid, name text)\\n RETURNS uuid\\n LANGUAGE c\\n IMMUTABLE PARALLEL SAFE STRICT\\nAS '$libdir/uuid-ossp', $function$uuid_generate_v3$function$\\n\",\"identity_arguments\":\"namespace uuid, name text\",\"language\":\"c\",\"leakproof\":false,\"name\":\"uuid_generate_v3\",\"parallel\":\"s\",\"result\":\"uuid\",\"security_definer\":false,\"volatility\":\"i\"}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "function" "uuid_generate_v5(namespace uuid, name text)" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"definition\":\"CREATE OR REPLACE FUNCTION hcl_advanced.uuid_generate_v5(namespace uuid, name text)\\n RETURNS uuid\\n LANGUAGE c\\n IMMUTABLE PARALLEL SAFE STRICT\\nAS '$libdir/uuid-ossp', $function$uuid_generate_v5$function$\\n\",\"identity_arguments\":\"namespace uuid, name text\",\"language\":\"c\",\"leakproof\":false,\"name\":\"uuid_generate_v5\",\"parallel\":\"s\",\"result\":\"uuid\",\"security_definer\":false,\"volatility\":\"i\"}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "function" "uuid_ns_dns()" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"definition\":\"CREATE OR REPLACE FUNCTION hcl_advanced.uuid_ns_dns()\\n RETURNS uuid\\n LANGUAGE c\\n IMMUTABLE PARALLEL SAFE STRICT\\nAS '$libdir/uuid-ossp', $function$uuid_ns_dns$function$\\n\",\"identity_arguments\":\"\",\"language\":\"c\",\"leakproof\":false,\"name\":\"uuid_ns_dns\",\"parallel\":\"s\",\"result\":\"uuid\",\"security_definer\":false,\"volatility\":\"i\"}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "grant" "PUBLIC:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "procedure:a1c4e4c751372a1f35bc1d62"
  spec_json = "{\"grantable\":false,\"grantee\":\"PUBLIC\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"procedure:a1c4e4c751372a1f35bc1d62\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "PUBLIC:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:01ce96415b496810ab48867d"
  spec_json = "{\"grantable\":false,\"grantee\":\"PUBLIC\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:01ce96415b496810ab48867d\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:insert:postgres" {
  schema    = "hcl_advanced"
  parent    = "view:04f3d22ad3913bf0d5c22979"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"INSERT\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"view:04f3d22ad3913bf0d5c22979\",\"type\":\"references\"}]"
}

resource "grant" "postgres:update:postgres" {
  schema    = "hcl_advanced"
  parent    = "table:3e9c73b9fd428a45efaa9c6d"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"UPDATE\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"table:3e9c73b9fd428a45efaa9c6d\",\"type\":\"references\"}]"
}

resource "grant" "PUBLIC:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:abdf61ff44646f7510a87fd6"
  spec_json = "{\"grantable\":false,\"grantee\":\"PUBLIC\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:abdf61ff44646f7510a87fd6\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:insert:postgres" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"INSERT\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"references\"}]"
}

resource "grant" "postgres:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:5596642ff0ca3efecba3b306"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:5596642ff0ca3efecba3b306\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:delete:postgres" {
  schema    = "hcl_advanced"
  parent    = "table:3e9c73b9fd428a45efaa9c6d"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"DELETE\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"table:3e9c73b9fd428a45efaa9c6d\",\"type\":\"references\"}]"
}

resource "grant" "postgres:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:8e8b3d44190f8b1ee93ab5b0"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:8e8b3d44190f8b1ee93ab5b0\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "hcl_advanced_reader:usage:postgres" {
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"grantable\":false,\"grantee\":\"hcl_advanced_reader\",\"grantor\":\"postgres\",\"privilege\":\"USAGE\"}"
  deps_json = "[{\"target\":\"role:56e576e5ae5de9009d76c962\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"references\"}]"
}

resource "grant" "PUBLIC:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:8e8b3d44190f8b1ee93ab5b0"
  spec_json = "{\"grantable\":false,\"grantee\":\"PUBLIC\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:8e8b3d44190f8b1ee93ab5b0\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:delete:postgres" {
  schema    = "hcl_advanced"
  parent    = "view:04f3d22ad3913bf0d5c22979"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"DELETE\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"view:04f3d22ad3913bf0d5c22979\",\"type\":\"references\"}]"
}

resource "grant" "postgres:trigger:postgres" {
  schema    = "hcl_advanced"
  parent    = "view:04f3d22ad3913bf0d5c22979"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"TRIGGER\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"view:04f3d22ad3913bf0d5c22979\",\"type\":\"references\"}]"
}

resource "grant" "postgres:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:4400ccb84e7abb8de6176e0d"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:4400ccb84e7abb8de6176e0d\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:references:postgres" {
  schema    = "hcl_advanced"
  parent    = "table:3e9c73b9fd428a45efaa9c6d"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"REFERENCES\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"table:3e9c73b9fd428a45efaa9c6d\",\"type\":\"references\"}]"
}

resource "grant" "hcl_advanced_reader:select:postgres" {
  schema    = "hcl_advanced"
  parent    = "view:04f3d22ad3913bf0d5c22979"
  spec_json = "{\"grantable\":false,\"grantee\":\"hcl_advanced_reader\",\"grantor\":\"postgres\",\"privilege\":\"SELECT\"}"
  deps_json = "[{\"target\":\"role:56e576e5ae5de9009d76c962\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"view:04f3d22ad3913bf0d5c22979\",\"type\":\"references\"}]"
}

resource "grant" "postgres:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:1a6d3be6b6ae679bf040e010"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:1a6d3be6b6ae679bf040e010\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:update:postgres" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"UPDATE\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"references\"}]"
}

resource "grant" "PUBLIC:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:57022e24e6dd703beef2fb18"
  spec_json = "{\"grantable\":false,\"grantee\":\"PUBLIC\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:57022e24e6dd703beef2fb18\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:update:postgres" {
  schema    = "hcl_advanced"
  parent    = "view:04f3d22ad3913bf0d5c22979"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"UPDATE\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"view:04f3d22ad3913bf0d5c22979\",\"type\":\"references\"}]"
}

resource "grant" "postgres:trigger:postgres" {
  schema    = "hcl_advanced"
  parent    = "table:3e9c73b9fd428a45efaa9c6d"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"TRIGGER\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"table:3e9c73b9fd428a45efaa9c6d\",\"type\":\"references\"}]"
}

resource "grant" "PUBLIC:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:3fcfb92f97c9cff3008c5ab8"
  spec_json = "{\"grantable\":false,\"grantee\":\"PUBLIC\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:3fcfb92f97c9cff3008c5ab8\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "PUBLIC:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:b174675a8055f0f4097a79ec"
  spec_json = "{\"grantable\":false,\"grantee\":\"PUBLIC\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:b174675a8055f0f4097a79ec\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:90dc81a745f28174cf482012"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:90dc81a745f28174cf482012\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "PUBLIC:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:4400ccb84e7abb8de6176e0d"
  spec_json = "{\"grantable\":false,\"grantee\":\"PUBLIC\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:4400ccb84e7abb8de6176e0d\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "PUBLIC:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:1a6d3be6b6ae679bf040e010"
  spec_json = "{\"grantable\":false,\"grantee\":\"PUBLIC\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:1a6d3be6b6ae679bf040e010\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:delete:postgres" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"DELETE\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"references\"}]"
}

resource "grant" "postgres:insert:postgres" {
  schema    = "hcl_advanced"
  parent    = "table:3e9c73b9fd428a45efaa9c6d"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"INSERT\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"table:3e9c73b9fd428a45efaa9c6d\",\"type\":\"references\"}]"
}

resource "grant" "postgres:select:postgres" {
  schema    = "hcl_advanced"
  parent    = "materialized_view:ea235edecb7a1a07a02b3bce"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"SELECT\"}"
  deps_json = "[{\"target\":\"materialized_view:ea235edecb7a1a07a02b3bce\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "PUBLIC:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:7648a90ccec227a98f2a3247"
  spec_json = "{\"grantable\":false,\"grantee\":\"PUBLIC\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:7648a90ccec227a98f2a3247\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:trigger:postgres" {
  schema    = "hcl_advanced"
  parent    = "materialized_view:ea235edecb7a1a07a02b3bce"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"TRIGGER\"}"
  deps_json = "[{\"target\":\"materialized_view:ea235edecb7a1a07a02b3bce\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "PUBLIC:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:6942d3f81acbfb463ae6f0c4"
  spec_json = "{\"grantable\":false,\"grantee\":\"PUBLIC\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:6942d3f81acbfb463ae6f0c4\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:usage:postgres" {
  schema    = "hcl_advanced"
  parent    = "sequence:faf92af04497c96d486afc26"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"USAGE\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"sequence:faf92af04497c96d486afc26\",\"type\":\"references\"}]"
}

resource "grant" "postgres:select:postgres" {
  schema    = "hcl_advanced"
  parent    = "table:3e9c73b9fd428a45efaa9c6d"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"SELECT\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"table:3e9c73b9fd428a45efaa9c6d\",\"type\":\"references\"}]"
}

resource "grant" "postgres:trigger:postgres" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"TRIGGER\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"references\"}]"
}

resource "grant" "postgres:references:postgres" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"REFERENCES\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"references\"}]"
}

resource "grant" "postgres:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:7648a90ccec227a98f2a3247"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:7648a90ccec227a98f2a3247\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:b174675a8055f0f4097a79ec"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:b174675a8055f0f4097a79ec\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "PUBLIC:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:90dc81a745f28174cf482012"
  spec_json = "{\"grantable\":false,\"grantee\":\"PUBLIC\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:90dc81a745f28174cf482012\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:truncate:postgres" {
  schema    = "hcl_advanced"
  parent    = "table:3e9c73b9fd428a45efaa9c6d"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"TRUNCATE\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"table:3e9c73b9fd428a45efaa9c6d\",\"type\":\"references\"}]"
}

resource "grant" "postgres:references:postgres" {
  schema    = "hcl_advanced"
  parent    = "view:04f3d22ad3913bf0d5c22979"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"REFERENCES\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"view:04f3d22ad3913bf0d5c22979\",\"type\":\"references\"}]"
}

resource "grant" "postgres:truncate:postgres" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"TRUNCATE\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"references\"}]"
}

resource "grant" "postgres:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:01ce96415b496810ab48867d"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:01ce96415b496810ab48867d\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:delete:postgres" {
  schema    = "hcl_advanced"
  parent    = "materialized_view:ea235edecb7a1a07a02b3bce"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"DELETE\"}"
  deps_json = "[{\"target\":\"materialized_view:ea235edecb7a1a07a02b3bce\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:abdf61ff44646f7510a87fd6"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:abdf61ff44646f7510a87fd6\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "PUBLIC:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:5596642ff0ca3efecba3b306"
  spec_json = "{\"grantable\":false,\"grantee\":\"PUBLIC\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:5596642ff0ca3efecba3b306\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:update:postgres" {
  schema    = "hcl_advanced"
  parent    = "materialized_view:ea235edecb7a1a07a02b3bce"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"UPDATE\"}"
  deps_json = "[{\"target\":\"materialized_view:ea235edecb7a1a07a02b3bce\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:select:postgres" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"SELECT\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"references\"}]"
}

resource "grant" "postgres:truncate:postgres" {
  schema    = "hcl_advanced"
  parent    = "view:04f3d22ad3913bf0d5c22979"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"TRUNCATE\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"view:04f3d22ad3913bf0d5c22979\",\"type\":\"references\"}]"
}

resource "grant" "postgres:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:6942d3f81acbfb463ae6f0c4"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:6942d3f81acbfb463ae6f0c4\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:create:postgres" {
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"CREATE\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"references\"}]"
}

resource "grant" "postgres:truncate:postgres" {
  schema    = "hcl_advanced"
  parent    = "materialized_view:ea235edecb7a1a07a02b3bce"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"TRUNCATE\"}"
  deps_json = "[{\"target\":\"materialized_view:ea235edecb7a1a07a02b3bce\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:references:postgres" {
  schema    = "hcl_advanced"
  parent    = "materialized_view:ea235edecb7a1a07a02b3bce"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"REFERENCES\"}"
  deps_json = "[{\"target\":\"materialized_view:ea235edecb7a1a07a02b3bce\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "procedure:a1c4e4c751372a1f35bc1d62"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"procedure:a1c4e4c751372a1f35bc1d62\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:3fcfb92f97c9cff3008c5ab8"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:3fcfb92f97c9cff3008c5ab8\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:execute:postgres" {
  schema    = "hcl_advanced"
  parent    = "function:57022e24e6dd703beef2fb18"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"EXECUTE\"}"
  deps_json = "[{\"target\":\"function:57022e24e6dd703beef2fb18\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "grant" "postgres:usage:postgres" {
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"USAGE\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"references\"}]"
}

resource "grant" "postgres:select:postgres" {
  schema    = "hcl_advanced"
  parent    = "view:04f3d22ad3913bf0d5c22979"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"SELECT\"}"
  deps_json = "[{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"},{\"target\":\"view:04f3d22ad3913bf0d5c22979\",\"type\":\"references\"}]"
}

resource "grant" "postgres:insert:postgres" {
  schema    = "hcl_advanced"
  parent    = "materialized_view:ea235edecb7a1a07a02b3bce"
  spec_json = "{\"grantable\":false,\"grantee\":\"postgres\",\"grantor\":\"postgres\",\"privilege\":\"INSERT\"}"
  deps_json = "[{\"target\":\"materialized_view:ea235edecb7a1a07a02b3bce\",\"type\":\"references\"},{\"target\":\"role:e4a945bdac015258acac9037\",\"type\":\"references\"}]"
}

resource "index" "accounts_active_org_idx" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"definition\":\"CREATE INDEX accounts_active_org_idx ON hcl_advanced.accounts USING btree (organization_id, created_at DESC) WHERE (status = 'active'::hcl_advanced.account_status)\",\"method\":\"btree\",\"ready\":true,\"unique\":false,\"valid\":true}"
  deps_json = "[{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"contains\"}]"
}

resource "materialized_view" "organization_account_counts" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"definition\":\" SELECT organization_id,\\n    count(*) AS account_count\\n   FROM hcl_advanced.accounts\\n  GROUP BY organization_id;\"}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"},{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"references\"}]"
}

resource "membership" "hcl_advanced_app->hcl_advanced_reader" {
  spec_json = "{\"admin\":false,\"member\":\"hcl_advanced_app\",\"parent\":\"hcl_advanced_reader\"}"
  deps_json = "[{\"target\":\"role:56e576e5ae5de9009d76c962\",\"type\":\"references\"},{\"target\":\"role:a65889270cec9a32d54efbe7\",\"type\":\"references\"}]"
}

resource "policy" "accounts_reader_policy" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"command\":\"r\",\"permissive\":true,\"roles\":[\"hcl_advanced_reader\"],\"using\":\"(status = 'active'::hcl_advanced.account_status)\"}"
  deps_json = "[{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"contains\"}]"
}

resource "primary_key" "organizations_pkey" {
  schema    = "hcl_advanced"
  parent    = "table:3e9c73b9fd428a45efaa9c6d"
  spec_json = "{\"deferrable\":false,\"definition\":\"PRIMARY KEY (id)\",\"initially_deferred\":false,\"validated\":true}"
  deps_json = "[{\"target\":\"table:3e9c73b9fd428a45efaa9c6d\",\"type\":\"contains\"}]"
}

resource "primary_key" "accounts_pkey" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"deferrable\":false,\"definition\":\"PRIMARY KEY (id)\",\"initially_deferred\":false,\"validated\":true}"
  deps_json = "[{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"contains\"}]"
}

resource "procedure" "activate_account(IN account_id bigint)" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"definition\":\"CREATE OR REPLACE PROCEDURE hcl_advanced.activate_account(IN account_id bigint)\\n LANGUAGE sql\\nAS $procedure$\\nUPDATE hcl_advanced.accounts SET status = 'active' WHERE id = account_id;\\n$procedure$\\n\",\"identity_arguments\":\"IN account_id bigint\",\"language\":\"sql\",\"leakproof\":false,\"name\":\"activate_account\",\"parallel\":\"u\",\"result\":\"\",\"security_definer\":false,\"volatility\":\"v\"}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "role" "hcl_advanced_reader" {
  spec_json = "{\"bypass_rls\":false,\"connection_limit\":-1,\"create_database\":false,\"create_role\":false,\"inherit\":true,\"login\":false,\"replication\":false,\"superuser\":false}"
}

resource "role" "hcl_advanced_app" {
  spec_json = "{\"bypass_rls\":false,\"connection_limit\":-1,\"create_database\":false,\"create_role\":false,\"inherit\":true,\"login\":false,\"replication\":false,\"superuser\":false}"
}

resource "role" "postgres" {
  spec_json = "{\"bypass_rls\":true,\"connection_limit\":-1,\"create_database\":true,\"create_role\":true,\"inherit\":true,\"login\":true,\"replication\":true,\"superuser\":true}"
}

resource "schema" "hcl_advanced" {
  spec_json = "{}"
}

resource "sequence" "account_id_seq" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"cache\":10,\"cycle\":false,\"increment\":1,\"max\":9223372036854776000,\"min\":1,\"start\":1000}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "table" "accounts" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"force_row_security\":true,\"partitioned\":false,\"persistence\":\"p\",\"row_security\":true}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "table" "organizations" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"force_row_security\":false,\"partitioned\":false,\"persistence\":\"p\",\"row_security\":false}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"}]"
}

resource "trigger" "accounts_normalize_email" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"definition\":\"CREATE TRIGGER accounts_normalize_email BEFORE INSERT OR UPDATE OF email ON hcl_advanced.accounts FOR EACH ROW EXECUTE FUNCTION hcl_advanced.set_normalized_email()\",\"enabled\":\"O\"}"
  deps_json = "[{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"contains\"}]"
}

resource "unique_constraint" "organizations_name_key" {
  schema    = "hcl_advanced"
  parent    = "table:3e9c73b9fd428a45efaa9c6d"
  spec_json = "{\"deferrable\":false,\"definition\":\"UNIQUE (name)\",\"initially_deferred\":false,\"validated\":true}"
  deps_json = "[{\"target\":\"table:3e9c73b9fd428a45efaa9c6d\",\"type\":\"contains\"}]"
}

resource "unique_constraint" "accounts_email_key" {
  schema    = "hcl_advanced"
  parent    = "table:0ffba3b03647679a95275fa1"
  spec_json = "{\"deferrable\":false,\"definition\":\"UNIQUE (email)\",\"initially_deferred\":false,\"validated\":true}"
  deps_json = "[{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"contains\"}]"
}

resource "view" "active_accounts" {
  schema    = "hcl_advanced"
  parent    = "schema:a025e821f1dbbae90052fb5b"
  spec_json = "{\"definition\":\" SELECT id,\\n    organization_id,\\n    email,\\n    created_at\\n   FROM hcl_advanced.accounts\\n  WHERE status = 'active'::hcl_advanced.account_status;\"}"
  deps_json = "[{\"target\":\"schema:a025e821f1dbbae90052fb5b\",\"type\":\"contains\"},{\"target\":\"table:0ffba3b03647679a95275fa1\",\"type\":\"references\"}]"
}

