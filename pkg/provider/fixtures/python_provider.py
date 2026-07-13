"""Minimal Python SDK fixture for autosql.provider/v1 conformance.

The host invokes extract with JSON request data; this fixture never receives a
database connection and emits canonical desired state plus source diagnostics.
"""
import json, sys

def extract(request):
    return {"document": {"version": "autosql.schema/v1", "graph": {"resources": []}},
            "diagnostics": [{"severity": "info", "code": "fixture", "message": "python provider"}],
            "cache_key": request.get("cache_key", "")}

if __name__ == "__main__":
    for line in sys.stdin:
        if line.strip(): print(json.dumps(extract(json.loads(line)), sort_keys=True), flush=True)
