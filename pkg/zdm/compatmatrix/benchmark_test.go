package compatmatrix

import (
	"context"
	"fmt"
	"testing"
)

func BenchmarkZDMTriggerWrite(b *testing.B) {
	ctx, url, c, cleanup := live(b)
	defer cleanup()
	install(b, ctx, url, c, 1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, e := c.Exec(ctx, "update mx_v1.items set name=$1 where id=$2", fmt.Sprintf("B%d", i), i%1000+1); e != nil {
			b.Fatal(e)
		}
	}
	b.ReportMetric(2, "physical-column-writes/op")
}
func BenchmarkZDMVersionRead(b *testing.B) {
	ctx, _, c, cleanup := live(b)
	defer cleanup()
	url := c.Config().ConnString()
	install(b, ctx, url, c, 1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var x string
		if e := c.QueryRow(context.Background(), "select name from mx_v2.items where id=$1", i%1000+1).Scan(&x); e != nil {
			b.Fatal(e)
		}
	}
}
