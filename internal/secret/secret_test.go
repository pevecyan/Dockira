package secret

import (
	"bytes"
	"errors"
	"testing"
)

var (
	keyA = bytes.Repeat([]byte{0xA5}, 32)
	keyB = bytes.Repeat([]byte{0x5A}, 32)
)

func mustBox(t *testing.T, key []byte, purpose string) *Box {
	t.Helper()
	b, err := NewBox(key, purpose)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	b := mustBox(t, keyA, "notify.channel")
	for _, pt := range [][]byte{nil, []byte("x"), []byte(`{"token":"tk_123"}`), bytes.Repeat([]byte("z"), 10000)} {
		sealed, err := b.Seal(pt)
		if err != nil {
			t.Fatal(err)
		}
		if len(pt) > 3 && bytes.Contains(sealed, pt) {
			t.Fatal("plaintext visible in sealed value")
		}
		got, err := b.Open(sealed)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("got %q, want %q", got, pt)
		}
	}
}

func TestNonceIsRandom(t *testing.T) {
	b := mustBox(t, keyA, "p")
	s1, _ := b.Seal([]byte("same"))
	s2, _ := b.Seal([]byte("same"))
	if bytes.Equal(s1, s2) {
		t.Fatal("two seals of the same value are identical")
	}
	if s1[0] != version {
		t.Fatalf("version byte = %d", s1[0])
	}
}

func TestWrongKeyFails(t *testing.T) {
	sealed, _ := mustBox(t, keyA, "p").Seal([]byte("secret"))
	if _, err := mustBox(t, keyB, "p").Open(sealed); !errors.Is(err, ErrOpen) {
		t.Fatalf("err = %v, want ErrOpen", err)
	}
}

func TestDifferentPurposesDifferentKeys(t *testing.T) {
	sealed, _ := mustBox(t, keyA, "notify.channel").Seal([]byte("secret"))
	if _, err := mustBox(t, keyA, "something.else").Open(sealed); !errors.Is(err, ErrOpen) {
		t.Fatalf("err = %v, want ErrOpen", err)
	}
}

func TestTamperFails(t *testing.T) {
	b := mustBox(t, keyA, "p")
	sealed, _ := b.Seal([]byte("secret value"))
	for i := 1; i < len(sealed); i++ { // byte 0 is the version, checked below
		c := bytes.Clone(sealed)
		c[i] ^= 0x01
		if _, err := b.Open(c); !errors.Is(err, ErrOpen) {
			t.Fatalf("flip at %d: err = %v", i, err)
		}
	}
	c := bytes.Clone(sealed)
	c[0] = 2
	if _, err := b.Open(c); err == nil {
		t.Fatal("unknown version accepted")
	}
	for _, short := range [][]byte{nil, sealed[:1], sealed[:20], sealed[:len(sealed)-1]} {
		if _, err := b.Open(short); err == nil {
			t.Fatalf("truncated value (%d bytes) accepted", len(short))
		}
	}
}

func TestNewBoxRejectsShortKey(t *testing.T) {
	if _, err := NewBox(make([]byte, 31), "p"); err == nil {
		t.Fatal("31-byte key accepted")
	}
	if _, err := NewBox(keyA, ""); err == nil {
		t.Fatal("empty purpose accepted")
	}
	if _, err := NewBox(make([]byte, 64), "p"); err != nil {
		t.Fatal(err)
	}
}
