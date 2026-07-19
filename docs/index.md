---
page_title: "AutoSQL Provider"
description: |-
  Apply immutable, approved AutoSQL schema and migration artifacts without
  placing database credentials in Terraform state.
---

# AutoSQL provider

The AutoSQL provider exposes `autosql_schema` and `autosql_migration`. Both
resources verify immutable artifact and approval digests, then delegate to the
released AutoSQL CLI. Database connections and approval material remain opaque
`env://` or absolute `file://` references.

```hcl
terraform {
  required_providers {
    autosql = {
      source  = "stigenai/autosql"
      version = "~> 0.1"
    }
  }
}

provider "autosql" {
  apply_config_ref = "file:///run/secrets/autosql/apply.json"
}
```

Set `binary_path` only when the released `autosql` executable is not available
on `PATH`. The provider never resolves the database connection reference into
Terraform state.
