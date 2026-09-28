package crypto

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestPassword(t *testing.T) {
	h, err := HashPassword("segredo123")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword("segredo123", h) || VerifyPassword("errada", h) {
		t.Fatal("verify mismatch")
	}
}

func TestStreamRoundtrip(t *testing.T) {
	for _, size := range []int{0, 1, chunkSize - 1, chunkSize, chunkSize*3 + 17} {
		data := make([]byte, size)
		rand.Read(data)
		var enc, dec bytes.Buffer
		if err := Encrypt(&enc, bytes.NewReader(data), "k"); err != nil {
			t.Fatal(err)
		}
		if err := Decrypt(&dec, bytes.NewReader(enc.Bytes()), "k"); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(dec.Bytes(), data) {
			t.Fatalf("size %d: mismatch", size)
		}
		if err := Decrypt(&bytes.Buffer{}, bytes.NewReader(enc.Bytes()), "x"); err == nil {
			t.Fatal("wrong key accepted")
		}
		if size > chunkSize {
			trunc := enc.Bytes()[:len(enc.Bytes())-(chunkSize/2)]
			if err := Decrypt(&bytes.Buffer{}, bytes.NewReader(trunc), "k"); err == nil {
				t.Fatal("truncation not detected")
			}
		}
	}
}
