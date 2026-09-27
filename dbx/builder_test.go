package dbx

import (
	"reflect"
	"strings"
	"testing"
)

func TestParameterizedCRUDBuilders(t *testing.T) {
	table, _ := Ident("public.users")
	id, _ := Ident("id")
	name, _ := Ident("name")
	password, _ := Ident("password_hash")

	query, args, err := BuildSelect(table, []Identifier{id, name}, []Predicate{Eq(id, "user-1"), Eq(password, nil)}, 25)
	if err != nil {
		t.Fatal(err)
	}
	if want := `SELECT "id", "name" FROM "public"."users" WHERE "id" = $1 AND "password_hash" IS NULL LIMIT $2`; query != want {
		t.Fatalf("select SQL\n got: %s\nwant: %s", query, want)
	}
	if !reflect.DeepEqual(args, []any{"user-1", 25}) {
		t.Fatalf("select args: %#v", args)
	}

	query, args, err = BuildInsert(table, []Assignment{{Column: name, Value: "Ada'); DROP TABLE users;--"}, {Column: password, Value: "secret-value"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `INSERT INTO "public"."users" ("name", "password_hash") VALUES ($1, $2)`; query != want {
		t.Fatalf("insert SQL\n got: %s\nwant: %s", query, want)
	}
	if !reflect.DeepEqual(args, []any{"Ada'); DROP TABLE users;--", "secret-value"}) {
		t.Fatalf("insert args: %#v", args)
	}

	query, args, err = BuildUpdate(table, []Assignment{{Column: name, Value: "Ada"}}, []Predicate{Eq(id, "user-1")})
	if err != nil {
		t.Fatal(err)
	}
	if want := `UPDATE "public"."users" SET "name" = $1 WHERE "id" = $2`; query != want {
		t.Fatalf("update SQL\n got: %s\nwant: %s", query, want)
	}
	if !reflect.DeepEqual(args, []any{"Ada", "user-1"}) {
		t.Fatalf("update args: %#v", args)
	}

	query, args, err = BuildDelete(table, []Predicate{Eq(id, "user-1")})
	if err != nil {
		t.Fatal(err)
	}
	if want := `DELETE FROM "public"."users" WHERE "id" = $1`; query != want {
		t.Fatalf("delete SQL\n got: %s\nwant: %s", query, want)
	}
	if !reflect.DeepEqual(args, []any{"user-1"}) {
		t.Fatalf("delete args: %#v", args)
	}
}

func TestCRUDBuilderRejectsUntrustedIdentifiersAndFullTableWrites(t *testing.T) {
	if _, err := Ident(`users; DROP TABLE users;--`); err == nil {
		t.Fatal("expected injected identifier to be rejected")
	}
	table, _ := Ident("users")
	column, _ := Ident("name")
	if _, _, err := BuildUpdate(table, []Assignment{{Column: column, Value: "x"}}, nil); err == nil {
		t.Fatal("expected unfiltered update to be rejected")
	}
	if _, _, err := BuildDelete(table, nil); err == nil {
		t.Fatal("expected unfiltered delete to be rejected")
	}
	query, _, err := BuildSelect(table, []Identifier{column}, nil, 0)
	if err != nil || strings.Contains(query, "WHERE") {
		t.Fatalf("unfiltered SELECT: %q, err=%v", query, err)
	}
}
