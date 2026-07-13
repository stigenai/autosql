package ide

import (
	"context"
	"testing"
)

type h struct{}

func (h) Handle(context.Context, Request) (Response, error) {
	return Response{Operation: Validate, Diagnostics: []Diagnostic{{URI: "schema.sql", Line: 2, Column: 3, Severity: "error", Code: "E1", Message: "bad"}}}, nil
}
func TestLocalProtocolAndProductionGuard(t *testing.T) {
	l := Local{Engine: h{}}
	r, e := l.Run(context.Background(), Request{Operation: Validate, Environment: "development"})
	if e != nil || len(r.Diagnostics) != 1 || r.Diagnostics[0].URI != "schema.sql" {
		t.Fatalf("%+v %v", r, e)
	}
	if _, e = l.Run(context.Background(), Request{Operation: Diff, Environment: "production"}); e != ErrProductionTarget {
		t.Fatalf("guard=%v", e)
	}
}
