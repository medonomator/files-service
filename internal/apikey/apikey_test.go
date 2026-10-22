package apikey

import (
	"bytes"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	first, err := Generate()
	if err != nil {
		t.Fatal(err)
	}

	second, err := Generate()
	if err != nil {
		t.Fatal(err)
	}

	if first.Plain == second.Plain {
		t.Fatal("keys must be random")
	}

	if !WellFormed(first.Plain) {
		t.Fatalf("generated key %q is not well formed", first.Plain)
	}

	if !bytes.Equal(first.Hash, Hash(first.Plain)) || len(first.Hash) != 32 {
		t.Fatal("hash must be sha256 of the plain key")
	}

	if !strings.HasPrefix(first.Plain, first.Display) || len(first.Display) != displayChars {
		t.Fatalf("display %q must be a short prefix of the key", first.Display)
	}
}

func TestWellFormed(t *testing.T) {
	for _, key := range []string{"", "fsk_", "fsk_short", "abc_" + strings.Repeat("A", 43), "fsk_" + strings.Repeat("*", 43)} {
		if WellFormed(key) {
			t.Fatalf("%q must be rejected", key)
		}
	}
}
