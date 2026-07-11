package source

import "testing"

func TestSplitSQLQuotesCommentsAndFunctions(t *testing.T) {
	input := `-- initial comment
CREATE TABLE public.users (id bigint, note text DEFAULT ';');
/* nested ; /* still nested */ done */
CREATE FUNCTION public.greet(text) RETURNS text LANGUAGE sql AS $body$
  SELECT 'hello; ' || $1;
$body$;
CREATE VIEW public.user_view AS SELECT "semi;colon" FROM public.users;`
	got, err := SplitSQL("schema.sql", input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d statements: %#v", len(got), got)
	}
	if got[0].Position.Line != 1 || got[0].Position.Column != 1 {
		t.Fatalf("unexpected first location: %+v", got[0].Position)
	}
	if got[1].Position.Line != 3 {
		t.Fatalf("unexpected function location: %+v", got[1].Position)
	}
}

func TestSplitSQLRejectsUnterminatedInput(t *testing.T) {
	for _, input := range []string{"SELECT 'x", "SELECT $tag$x", "/* x"} {
		if _, err := SplitSQL("bad.sql", input); err == nil {
			t.Fatalf("expected error for %q", input)
		}
	}
}
