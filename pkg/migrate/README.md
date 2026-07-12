# Migration directories

AutoSQL migration directories are immutable, ordered SQL histories described by
`.autosql-manifest.json`. Updates validate the complete candidate in memory,
stage byte-exact SQL, synchronize it, and expose the manifest last. Verification
must succeed before consumers use any migration. Non-linear history requires an
explicit `nonlinear` authorization and all parent versions.
