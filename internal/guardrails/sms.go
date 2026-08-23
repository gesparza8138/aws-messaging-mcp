package guardrails

import (
	"fmt"
	"strconv"
	"strings"
)

// smsMediaTypes are the MMS content types carriers accept reliably (PRD §8).
var smsMediaTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
}

// DestinationCountryUS requires a US E.164 destination (+1 then a 10-digit
// national number with a valid area code). This is the enforced counterpart
// of the protect configuration's US allow rules, which do not block
// countries they don't list.
func DestinationCountryUS(number string) Decision {
	name := "destination_country"
	n := strings.TrimSpace(number)
	ok := len(n) == 12 && strings.HasPrefix(n, "+1") && n[2] >= '2' && n[2] <= '9'
	if ok {
		for _, c := range n[2:] {
			if c < '0' || c > '9' {
				ok = false
				break
			}
		}
	}
	if ok {
		return Decision{Name: name, Allowed: true}
	}
	return Decision{Name: name, Allowed: false,
		Reason: fmt.Sprintf("destination %q is not a US E.164 number (+1XXXXXXXXXX)", number)}
}

// MaxPriceCapped returns the effective per-message price ceiling: the
// requested value when it parses and stays at or under the server ceiling,
// otherwise the ceiling itself ("the server caps at the configured maximum",
// PRD §5.3). Unparseable requests are blocked rather than guessed at.
func MaxPriceCapped(requested, ceiling string) (string, Decision) {
	name := "max_price"
	if strings.TrimSpace(requested) == "" {
		return ceiling, Decision{Name: name, Allowed: true, Reason: "server ceiling applied"}
	}
	req, err := strconv.ParseFloat(requested, 64)
	if err != nil || req <= 0 {
		return ceiling, Decision{Name: name, Allowed: false,
			Reason: fmt.Sprintf("MaxPrice %q is not a positive decimal", requested)}
	}
	limit, err := strconv.ParseFloat(ceiling, 64)
	if err != nil {
		return ceiling, Decision{Name: name, Allowed: false,
			Reason: fmt.Sprintf("server price ceiling %q is misconfigured", ceiling)}
	}
	if req > limit {
		return ceiling, Decision{Name: name, Allowed: true,
			Reason: fmt.Sprintf("requested %s capped to the server ceiling %s", requested, ceiling)}
	}
	return requested, Decision{Name: name, Allowed: true}
}

// OriginationAllowed pins the origination identity to the server's configured
// number (PRD S1). Empty input means "use the default" and passes.
func OriginationAllowed(requested, configured string) Decision {
	name := "origination_identity"
	r := strings.TrimSpace(requested)
	if r == "" || r == configured {
		return Decision{Name: name, Allowed: true}
	}
	return Decision{Name: name, Allowed: false,
		Reason: fmt.Sprintf("origination identity %q is not the configured number", requested)}
}

// MediaURLsInBucket requires every media URL to live in the server-owned
// media bucket (PRD §8): s3://<bucket>/<key>.
func MediaURLsInBucket(urls []string, bucket string) Decision {
	name := "media_bucket"
	prefix := "s3://" + bucket + "/"
	for _, u := range urls {
		if bucket == "" || !strings.HasPrefix(u, prefix) || len(u) == len(prefix) {
			return Decision{Name: name, Allowed: false,
				Reason: fmt.Sprintf("media URL %q is not inside the server media bucket", u)}
		}
	}
	return Decision{Name: name, Allowed: true}
}

// MediaUploadAllowed validates the inline-upload convenience: an accepted
// image type and a decoded size within the ceiling (PRD §8: ≤ 5 MB,
// jpeg/png/gif).
func MediaUploadAllowed(contentType string, sizeBytes, maxBytes int) Decision {
	name := "media_type_size"
	if !smsMediaTypes[strings.ToLower(strings.TrimSpace(contentType))] {
		return Decision{Name: name, Allowed: false,
			Reason: fmt.Sprintf("content type %q is not an accepted MMS image type (jpeg/png/gif)", contentType)}
	}
	if sizeBytes <= 0 || sizeBytes > maxBytes {
		return Decision{Name: name, Allowed: false,
			Reason: fmt.Sprintf("media is %d bytes; the limit is %d", sizeBytes, maxBytes)}
	}
	return Decision{Name: name, Allowed: true}
}
