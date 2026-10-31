This is a verified, two-version migration snapshot for disposable tests. Version
1 creates `project_demo.accounts`; version 2 adds a non-null label with a default.
The upgrade test seeds version 1 and checks both the row and its new label after
version 2. SQL and the manifest were written together by `pkg/migrate.Update`.

Run `autosql migrate test --config examples/project-testing/container.hcl --json`
after approving the example daemon and pinned image, or use `development.hcl`
with your approved development administrator. These unsigned fixture migrations
are test inputs, not production release artifacts or Apply authorization.

The `.autosql-manifest.json` file and its `.autosql-generations/` directory
belong together. Do not hand-edit the captured SQL or manifest. Configuration
validation alone does not execute these migrations.
