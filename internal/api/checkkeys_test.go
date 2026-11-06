package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// checkKeysByTokens is the json.Decoder version checkKeys replaced. The
// fuzz test below checks that the two agree.
func checkKeysByTokens(data []byte, fields map[string]uint) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errors.New("body must be a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := t.(string)
		if _, known := fields[key]; !known {
			return fmt.Errorf("unknown field %q", key)
		}
		if seen[key] {
			return fmt.Errorf("repeated field %q", key)
		}
		seen[key] = true
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("body must hold exactly one JSON object")
	}
	return nil
}

var checkKeysBodies = []string{
	`{"symbol":"BTC-USD","side":"buy","type":"limit","price":100,"qty":1}`,
	` { "qty" : 1 , "price" : -2.5e3 , "client_id" : "a\"b\\cé" } `,
	`{}`, `{"qty":{"a":[1,{"b":"}"}]}}`, `{"qty":[1,2,"]"]}`, `{"qty":null,"tif":true,"side":false}`,
	`{"QTY":1}`, `{"qty":1,"qty":2}`, `{"nope":1}`, `{"a\"b":1}`,
	``, `null`, `[]`, `"x"`, `{}{}`, `{"qty":1}x`, `{"qty":}`, `{"qty":1,}`, `{qty:1}`, `{"qty"`,
}

func TestCheckKeys(t *testing.T) {
	for _, body := range checkKeysBodies {
		want, got := checkKeysByTokens([]byte(body), orderFields), checkKeys([]byte(body), orderFields)
		if (want == nil) != (got == nil) || isFieldError(want) && got.Error() != want.Error() {
			t.Errorf("%s: checkKeys = %v, the json.Decoder version %v", body, got, want)
		}
	}
}

func FuzzCheckKeys(f *testing.F) {
	for _, body := range checkKeysBodies {
		f.Add([]byte(body))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		want, got := checkKeysByTokens(data, orderFields), checkKeys(data, orderFields)
		// Both refuse invalid JSON, with messages of their own.
		if (want == nil) != (got == nil) || json.Valid(data) && isFieldError(want) && got.Error() != want.Error() {
			t.Fatalf("%q: checkKeys = %v, the json.Decoder version %v", data, got, want)
		}
	})
}

func isFieldError(err error) bool {
	return err != nil && (strings.HasPrefix(err.Error(), "unknown field") || strings.HasPrefix(err.Error(), "repeated field"))
}

func BenchmarkCheckKeys(b *testing.B) {
	body := []byte(`{"symbol":"BTC-USD","side":"buy","type":"limit","price":10002,"qty":37}`)
	for _, impl := range []struct {
		name string
		fn   func([]byte, map[string]uint) error
	}{{"tokens", checkKeysByTokens}, {"scan", checkKeys}} {
		b.Run(impl.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := impl.fn(body, orderFields); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
