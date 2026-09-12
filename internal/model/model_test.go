package model

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestMetadataLimitsAndPaths(t *testing.T) {
	good := Metadata{Description: strings.Repeat("è", 1000), OriginalName: strings.Repeat("界", 255)}
	encoded, err := EncodeMetadata(good)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeMetadata(encoded)
	if err != nil || decoded != good {
		t.Fatalf("roundtrip: %+v %v", decoded, err)
	}
	for _, m := range []Metadata{{Description: "", OriginalName: "dump.sql"}, {Description: strings.Repeat("x", 1001), OriginalName: "dump.sql"}, {Description: "db", OriginalName: "../dump.sql"}, {Description: "db", OriginalName: "C:\\dump.sql"}, {Description: "db", OriginalName: "."}, {Description: "db", OriginalName: "a\x00.sql"}, {Description: "db", OriginalName: strings.Repeat("x", 256)}} {
		if m.Validate() == nil {
			t.Errorf("accepted invalid metadata: %+v", m)
		}
	}
	for _, name := range []string{"backup.EXE", "backup.sql.gz", "backup", "report con spazi.zip"} {
		if err := (Metadata{Description: "database", OriginalName: name}).Validate(); err != nil {
			t.Errorf("unexpected extension restriction: %v", err)
		}
	}
	for _, format := range []string{"age-v2", "plain", " AGE-v1 "} {
		if err := (Metadata{Description: "db", OriginalName: "dump.sql", ContentFormat: format}).Validate(); err == nil {
			t.Errorf("accepted unsupported content format %q", format)
		}
	}
	ageMetadata := Metadata{Description: "db", OriginalName: "dump.sql", ContentFormat: ContentFormatAgeV1}
	encoded, err = EncodeMetadata(ageMetadata)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = DecodeMetadata(encoded)
	if err != nil || decoded != ageMetadata {
		t.Fatalf("age metadata roundtrip: %+v %v", decoded, err)
	}
	for _, raw := range []string{`{"description":"db","original_name":"x","extra":1}`, `{"description":"db","original_name":"x"}{}`, `{"description":"db","original_name":"x"}junk`} {
		if _, err := DecodeMetadata(base64.RawURLEncoding.EncodeToString([]byte(raw))); err == nil {
			t.Errorf("accepted extra JSON: %s", raw)
		}
	}
}
func TestTokenAndID(t *testing.T) {
	a, err := NewToken()
	if err != nil || !ValidToken(a) {
		t.Fatal(a, err)
	}
	b, _ := NewToken()
	if a == b || TokenHash(a) == a {
		t.Fatal("invalid credential generation")
	}
	id, _ := NewID()
	if !ValidID(id) || ValidID("../../x") {
		t.Fatal("ID validation")
	}
	if ValidToken("a b") || ValidToken(strings.Repeat("a", 256)) {
		t.Fatal("token limits")
	}
	idempotencyKey, err := NewIdempotencyKey()
	if err != nil || !ValidIdempotencyKey(idempotencyKey) {
		t.Fatal(idempotencyKey, err)
	}
	for _, invalid := range []string{"short", "contains space here", strings.Repeat("x", 129), "abcdefghijklmnop!"} {
		if ValidIdempotencyKey(invalid) {
			t.Fatalf("accepted invalid idempotency key %q", invalid)
		}
	}
}
func TestSizes(t *testing.T) {
	for s, want := range map[string]int64{"1GiB": 1 << 30, "20GB": 20000000000, "0": 0, "123": 123, "1TiB": 1 << 40} {
		got, err := ParseBytes(s)
		if err != nil || got != want {
			t.Errorf("%s => %d, %v", s, got, err)
		}
	}
	for _, s := range []string{"-1", "1.5GB", "huge", "9999999999999999999999999999999999TiB", ""} {
		if _, err := ParseBytes(s); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
}
