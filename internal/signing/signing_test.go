package signing

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" // #nosec G505 -- verifying the protocol-mandated digest
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testSigner(t *testing.T) *Signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &Signer{KeyPairID: "KTESTKEYID", PrivateKey: key}
}

// decodeCF reverses CloudFront's URL-safe base64 substitutions.
func decodeCF(t *testing.T, s string) []byte {
	t.Helper()
	std := strings.NewReplacer("-", "+", "_", "=", "~", "/").Replace(s)
	raw, err := base64.StdEncoding.DecodeString(std)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return raw
}

func TestCannedSignatureVerifies(t *testing.T) {
	s := testSigner(t)
	expires := time.Unix(1790000000, 0)
	target := "https://mcp.example.com/files/shared/u/report.pdf"
	signed, err := s.SignedURL(target, expires, "")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("Expires") != "1790000000" || q.Get("Key-Pair-Id") != "KTESTKEYID" || q.Get("Policy") != "" {
		t.Fatalf("canned params: %v", q)
	}
	policy := cannedPolicy(target, expires)
	digest := sha1.Sum([]byte(policy)) // #nosec G401
	sig := decodeCF(t, q.Get("Signature"))
	if err := rsa.VerifyPKCS1v15(&s.PrivateKey.PublicKey, crypto.SHA1, digest[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	for _, c := range []string{"+", "=", "/"} {
		if strings.Contains(q.Get("Signature"), c) {
			t.Fatalf("unsafe character %q in signature", c)
		}
	}
}

func TestCustomPolicyCarriesIPAndVerifies(t *testing.T) {
	s := testSigner(t)
	expires := time.Unix(1790000000, 0)
	target := "https://mcp.example.com/files/shared/u/a.zip?x=1"
	signed, err := s.SignedURL(target, expires, "203.0.113.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(signed, "?x=1&Policy=") {
		t.Fatalf("query separator wrong: %s", signed)
	}
	u, _ := url.Parse(signed)
	params := u.Query()
	policy := string(decodeCF(t, params.Get("Policy")))
	if !strings.Contains(policy, `"AWS:SourceIp":"203.0.113.0/24"`) ||
		!strings.Contains(policy, `"AWS:EpochTime":1790000000`) {
		t.Fatalf("policy: %s", policy)
	}
	digest := sha1.Sum([]byte(policy)) // #nosec G401
	if err := rsa.VerifyPKCS1v15(&s.PrivateKey.PublicKey, crypto.SHA1, digest[:], decodeCF(t, params.Get("Signature"))); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	if params.Get("Expires") != "" {
		t.Fatal("custom policy must not also carry Expires")
	}
}

func TestParsePrivateKeyPEM(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(key)
	for name, block := range map[string]*pem.Block{
		"pkcs8": {Type: "PRIVATE KEY", Bytes: pkcs8},
		"pkcs1": {Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)},
	} {
		parsed, err := ParsePrivateKeyPEM(pem.EncodeToMemory(block))
		if err != nil || parsed.N.Cmp(key.N) != 0 {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := ParsePrivateKeyPEM([]byte("not pem")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestSignerUnconfigured(t *testing.T) {
	if _, err := (&Signer{}).SignedURL("https://x/y", time.Now(), ""); err == nil {
		t.Fatal("unconfigured signer must refuse")
	}
}
