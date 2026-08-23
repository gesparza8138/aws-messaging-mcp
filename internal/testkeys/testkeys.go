// Package testkeys mints Cognito-shaped access tokens for tests with a
// locally generated RSA key standing in for the user pool's JWKS.
package testkeys

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/gesparza8138/aws-messaging-mcp/internal/auth"
)

// Keys is a signing key plus the public set a verifier resolves.
type Keys struct {
	Issuer   string
	ClientID string
	private  jwk.Key
	set      jwk.Set
}

// New generates a 2048-bit RSA key pair for issuer/clientID.
func New(issuer, clientID string) (*Keys, error) {
	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	private, err := jwk.Import(raw)
	if err != nil {
		return nil, err
	}
	if err := private.Set(jwk.KeyIDKey, "test-kid"); err != nil {
		return nil, err
	}
	if err := private.Set(jwk.AlgorithmKey, jwa.RS256()); err != nil {
		return nil, err
	}
	public, err := jwk.PublicKeyOf(private)
	if err != nil {
		return nil, err
	}
	set := jwk.NewSet()
	if err := set.AddKey(public); err != nil {
		return nil, err
	}
	return &Keys{Issuer: issuer, ClientID: clientID, private: private, set: set}, nil
}

// Provider returns a KeySetProvider serving the public set.
func (k *Keys) Provider() auth.KeySetProvider {
	return auth.KeySetFunc(func(context.Context) (jwk.Set, error) { return k.set, nil })
}

// Claims overrides default token claims; a nil value deletes the claim.
type Claims map[string]any

// Mint signs an access token with the defaults overridden by c.
func (k *Keys) Mint(c Claims) (string, error) {
	values := Claims{
		"sub":       "integration-user",
		"iss":       k.Issuer,
		"exp":       time.Now().Add(5 * time.Minute).Unix(),
		"token_use": "access",
		"client_id": k.ClientID,
		"scope":     "msg/read",
	}
	for key, v := range c {
		if v == nil {
			delete(values, key)
		} else {
			values[key] = v
		}
	}
	tok := jwt.New()
	for key, v := range values {
		if err := tok.Set(key, v); err != nil {
			return "", err
		}
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), k.private))
	if err != nil {
		return "", err
	}
	return string(signed), nil
}
