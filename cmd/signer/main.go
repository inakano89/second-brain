// Command signer manages the ed25519 key used to sign release checksums.
//
//	go run ./cmd/signer keygen                       # prints public/private keys (base64)
//	UPDATE_SIGNING_KEY=... go run ./cmd/signer sign dist/SHA256SUMS   # writes dist/SHA256SUMS.sig
//	go run ./cmd/signer verify dist/SHA256SUMS <public-key>
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "keygen":
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		check(err)
		fmt.Println("public (variável do repositório UPDATE_PUBLIC_KEY):")
		fmt.Println(base64.StdEncoding.EncodeToString(pub))
		fmt.Println("private (secret do repositório UPDATE_SIGNING_KEY — não compartilhe):")
		fmt.Println(base64.StdEncoding.EncodeToString(priv.Seed()))
	case "sign":
		if len(os.Args) != 3 {
			usage()
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("UPDATE_SIGNING_KEY")))
		check(err)
		var priv ed25519.PrivateKey
		switch len(raw) {
		case ed25519.SeedSize:
			priv = ed25519.NewKeyFromSeed(raw)
		case ed25519.PrivateKeySize:
			priv = ed25519.PrivateKey(raw)
		default:
			fail("UPDATE_SIGNING_KEY inválida")
		}
		data, err := os.ReadFile(os.Args[2])
		check(err)
		sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data))
		check(os.WriteFile(os.Args[2]+".sig", []byte(sig+"\n"), 0o644))
		fmt.Println("assinado:", os.Args[2]+".sig")
	case "verify":
		if len(os.Args) != 4 {
			usage()
		}
		data, err := os.ReadFile(os.Args[2])
		check(err)
		sigB64, err := os.ReadFile(os.Args[2] + ".sig")
		check(err)
		sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigB64)))
		check(err)
		pub, err := base64.StdEncoding.DecodeString(os.Args[3])
		check(err)
		if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, data, sig) {
			fail("assinatura inválida")
		}
		fmt.Println("assinatura válida")
	default:
		usage()
	}
}

func usage() {
	fail("uso: signer keygen | sign <arquivo> | verify <arquivo> <chave-pública>")
}

func check(err error) {
	if err != nil {
		fail(err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
