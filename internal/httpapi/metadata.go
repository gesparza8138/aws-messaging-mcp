package httpapi

import "github.com/gesparza8138/aws-messaging-mcp/internal/settings"

// ProtectedResourceDocument is the RFC 9728 protected-resource metadata
// (PRD A5), honoring AuthMetadataMode.
func ProtectedResourceDocument(s settings.Settings) map[string]any {
	authorizationServer := s.CognitoIssuer
	if s.AuthMetadataMode == "fronted" {
		authorizationServer = s.PublicBaseURL + "/oauth"
	}
	return map[string]any{
		"resource":                 s.MCPResourceURL,
		"authorization_servers":    []string{authorizationServer},
		"scopes_supported":         settings.ScopesSupported,
		"bearer_methods_supported": []string{"header"},
	}
}

// AuthorizationServerDocument is RFC 8414 metadata mirroring Cognito's
// endpoints plus code_challenge_methods_supported, which Cognito omits and
// some clients require for PKCE (PRD R2). Served in both modes; the
// protected-resource document points here only in fronted mode.
func AuthorizationServerDocument(s settings.Settings) map[string]any {
	scopes := append([]string{"openid"}, settings.ScopesSupported...)
	return map[string]any{
		"issuer":                                s.CognitoIssuer,
		"authorization_endpoint":                s.CognitoDomain + "/oauth2/authorize",
		"token_endpoint":                        s.CognitoDomain + "/oauth2/token",
		"revocation_endpoint":                   s.CognitoDomain + "/oauth2/revoke",
		"jwks_uri":                              s.JWKSURL(),
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      scopes,
		"code_challenge_methods_supported":      []string{"S256"},
	}
}
