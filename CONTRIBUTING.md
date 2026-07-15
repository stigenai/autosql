# Contributing to AutoSQL

Start with the [AutoSQL Constitution](CONSTITUTION.md). Examples, demos, and
automated evidence are part of feature completeness, not optional follow-up
documentation.

Before opening a pull request for a feature change:

1. create or update its Beads issue;
2. update the implementation and compatibility behavior;
3. update the relevant user documentation;
4. add or expand executable example/demo evidence;
5. update [`examples/catalog.json`](examples/catalog.json) and the human
   [`examples/README.md`](examples/README.md); and
6. run `go test ./...`, `go vet ./...`, and the feature's documented demo
   command.

The repository test suite rejects production packages and advertised
PostgreSQL capabilities that are not represented in the feature catalog.
Evidence levels and exception rules are defined by the constitution.
