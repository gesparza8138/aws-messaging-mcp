// Package signing produces CloudFront signed URLs for the files bucket
// (PRD §5.3, M4b). The signature scheme — RSA over a SHA-1 digest of the
// policy JSON, with CloudFront's URL-safe base64 alphabet — is fixed by the
// CDN's trusted-key-group contract (M4b-4), not an integrity choice here.
package signing

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" // #nosec G505 -- CloudFront signed URLs require SHA-1 (M4b-4)
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Signer signs URLs for one key pair registered in the distribution's
// trusted key group.
type Signer struct {
	KeyPairID  string
	PrivateKey *rsa.PrivateKey
}

// ParsePrivateKeyPEM accepts PKCS#8 or PKCS#1 RSA private keys.
func ParsePrivateKeyPEM(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("PEM holds a non-RSA key")
		}
		return rsaKey, nil
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// SignedURL returns rawURL with CloudFront signature parameters appended.
// An empty ipCIDR produces a canned policy (Expires); a non-empty one
// produces a custom policy carrying the IpAddress condition (PRD R9).
func (s *Signer) SignedURL(rawURL string, expires time.Time, ipCIDR string) (string, error) {
	if s.PrivateKey == nil || s.KeyPairID == "" {
		return "", errors.New("signer is not configured")
	}
	policy := cannedPolicy(rawURL, expires)
	if ipCIDR != "" {
		policy = customPolicy(rawURL, expires, ipCIDR)
	}
	digest := sha1.Sum([]byte(policy)) // #nosec G401 -- protocol requirement (M4b-4)
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.PrivateKey, crypto.SHA1, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign policy: %w", err)
	}
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	if ipCIDR != "" {
		return fmt.Sprintf("%s%sPolicy=%s&Signature=%s&Key-Pair-Id=%s",
			rawURL, sep, cloudFrontB64(policy), cloudFrontB64(string(signature)), s.KeyPairID), nil
	}
	return fmt.Sprintf("%s%sExpires=%d&Signature=%s&Key-Pair-Id=%s",
		rawURL, sep, expires.Unix(), cloudFrontB64(string(signature)), s.KeyPairID), nil
}

// cannedPolicy and customPolicy build the exact byte sequences CloudFront
// expects — the signature covers these bytes, so no JSON marshalling that
// could reorder or add whitespace.
func cannedPolicy(url string, expires time.Time) string {
	return fmt.Sprintf(
		`{"Statement":[{"Resource":%q,"Condition":{"DateLessThan":{"AWS:EpochTime":%d}}}]}`,
		url, expires.Unix())
}

func customPolicy(url string, expires time.Time, ipCIDR string) string {
	return fmt.Sprintf(
		`{"Statement":[{"Resource":%q,"Condition":{"IpAddress":{"AWS:SourceIp":%q},"DateLessThan":{"AWS:EpochTime":%d}}}]}`,
		url, ipCIDR, expires.Unix())
}

// cloudFrontB64 is standard base64 with CloudFront's substitutions for the
// characters that are unsafe in URLs: + → -, = → _, / → ~.
func cloudFrontB64(s string) string {
	out := base64.StdEncoding.EncodeToString([]byte(s))
	return strings.NewReplacer("+", "-", "=", "_", "/", "~").Replace(out)
}
