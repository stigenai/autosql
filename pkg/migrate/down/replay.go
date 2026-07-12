package down

import (
	"autosql/pkg/migrate"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/simulate"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/url"
	"time"
)

func ReplayPrior(ctx context.Context, s migrate.Snapshot, to, devURL, devIdentity, productionIdentity string, schemas []string) (doc schema.Document, err error) {
	v, ve := migrate.ParseVersion(to)
	if ve != nil {
		return doc, ErrRefused
	}
	to = v.String()
	actual, e := simulate.ResolvePostgresIdentity(ctx, devURL)
	if e != nil || actual != devIdentity || actual == productionIdentity {
		return doc, ErrRefused
	}
	u, e := url.Parse(devURL)
	if e != nil {
		return doc, ErrRefused
	}
	admin, e := pgx.Connect(ctx, devURL)
	if e != nil {
		return doc, ErrRefused
	}
	defer admin.Close(context.Background())
	random := make([]byte, 12)
	if _, e = rand.Read(random); e != nil {
		return doc, ErrRefused
	}
	name := "autosql_down_replay_" + hex.EncodeToString(random)
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); e != nil {
		return doc, ErrRefused
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, ce := admin.Exec(c, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		if ce != nil {
			err = errors.Join(err, ce)
		}
	}()
	du := *u
	du.Path = "/" + name
	conn, e := pgx.Connect(ctx, du.String())
	if e != nil {
		return doc, ErrRefused
	}
	defer conn.Close(context.Background())
	found := false
	for _, entry := range s.Manifest.Entries {
		if _, e = conn.Exec(ctx, string(s.Files[entry.File])); e != nil {
			return doc, ErrRefused
		}
		if entry.Version == to {
			found = true
			break
		}
	}
	if !found {
		return doc, ErrRefused
	}
	doc, e = postgres.InspectURL(ctx, du.String(), postgres.Options{Schemas: schemas})
	if e != nil {
		return schema.Document{}, ErrRefused
	}
	return postgres.New().Normalize(ctx, doc)
}
