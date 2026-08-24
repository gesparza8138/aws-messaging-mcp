package guardrails

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// riskyContentTypes never leave the server as shareable downloads (PRD §5.3):
// HTML executes in the browser on our origin, the rest execute on the
// recipient's machine.
var riskyContentTypes = map[string]bool{
	"text/html":                                     true,
	"application/xhtml+xml":                         true,
	"image/svg+xml":                                 true,
	"application/x-msdownload":                      true,
	"application/x-executable":                      true,
	"application/x-elf":                             true,
	"application/x-mach-binary":                     true,
	"application/x-sh":                              true,
	"application/x-bat":                             true,
	"application/java-archive":                      true,
	"application/vnd.microsoft.portable-executable": true,
}

// ContentTypeAllowed applies the deny-list; allowRisky (dev only, stack
// parameter) bypasses it for testing.
func ContentTypeAllowed(contentType string, allowRisky bool) Decision {
	name := "content_type"
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if ct == "" {
		return Decision{Name: name, Allowed: false, Reason: "ContentType is required"}
	}
	if riskyContentTypes[ct] && !allowRisky {
		return Decision{Name: name, Allowed: false,
			Reason: fmt.Sprintf("content type %q is on the deny-list (html/executables)", contentType)}
	}
	return Decision{Name: name, Allowed: true}
}

// LinkExpiry resolves exactly-one-of ExpiresIn (ISO-8601 duration) /
// DateLessThan (RFC 3339) into an absolute expiry no further than maxDays
// out (PRD §5.3).
func LinkExpiry(expiresIn, dateLessThan string, maxDays int, now time.Time) (time.Time, Decision) {
	name := "link_expiry"
	if (expiresIn == "") == (dateLessThan == "") {
		return time.Time{}, Decision{Name: name, Allowed: false,
			Reason: "exactly one of ExpiresIn or DateLessThan is required"}
	}
	var expiry time.Time
	if expiresIn != "" {
		d, err := parseISODuration(expiresIn)
		if err != nil {
			return time.Time{}, Decision{Name: name, Allowed: false,
				Reason: fmt.Sprintf("ExpiresIn %q: %v", expiresIn, err)}
		}
		expiry = now.Add(d)
	} else {
		t, err := time.Parse(time.RFC3339, dateLessThan)
		if err != nil {
			return time.Time{}, Decision{Name: name, Allowed: false,
				Reason: fmt.Sprintf("DateLessThan %q is not RFC 3339", dateLessThan)}
		}
		expiry = t
	}
	if !expiry.After(now) {
		return time.Time{}, Decision{Name: name, Allowed: false, Reason: "expiry is in the past"}
	}
	if limit := now.Add(time.Duration(maxDays) * 24 * time.Hour); expiry.After(limit) {
		return time.Time{}, Decision{Name: name, Allowed: false,
			Reason: fmt.Sprintf("expiry exceeds the maximum of %d days", maxDays)}
	}
	return expiry, Decision{Name: name, Allowed: true}
}

// parseISODuration handles the practical subset PnDTnHnMnS (weeks via nW).
func parseISODuration(s string) (time.Duration, error) {
	orig := s
	if !strings.HasPrefix(s, "P") || len(s) < 2 {
		return 0, fmt.Errorf("not an ISO-8601 duration")
	}
	s = s[1:]
	datePart, timePart, _ := strings.Cut(s, "T")
	var total time.Duration
	parse := func(part string, units map[byte]time.Duration) error {
		num := ""
		for i := 0; i < len(part); i++ {
			c := part[i]
			if c >= '0' && c <= '9' {
				num += string(c)
				continue
			}
			unit, ok := units[c]
			if !ok || num == "" {
				return fmt.Errorf("unexpected %q", string(c))
			}
			n, err := strconv.Atoi(num)
			if err != nil {
				return err
			}
			total += time.Duration(n) * unit
			num = ""
		}
		if num != "" {
			return fmt.Errorf("trailing number without unit")
		}
		return nil
	}
	if err := parse(datePart, map[byte]time.Duration{'D': 24 * time.Hour, 'W': 7 * 24 * time.Hour}); err != nil {
		return 0, fmt.Errorf("%q: %w", orig, err)
	}
	if err := parse(timePart, map[byte]time.Duration{'H': time.Hour, 'M': time.Minute, 'S': time.Second}); err != nil {
		return 0, fmt.Errorf("%q: %w", orig, err)
	}
	if total <= 0 {
		return 0, fmt.Errorf("%q: zero duration", orig)
	}
	return total, nil
}

// SizeWithin caps a payload size (inline bodies, declared upload lengths).
func SizeWithin(kind string, size, maximum int64) Decision {
	name := kind + "_size"
	if size <= 0 {
		return Decision{Name: name, Allowed: false, Reason: "size must be positive"}
	}
	if size > maximum {
		return Decision{Name: name, Allowed: false,
			Reason: fmt.Sprintf("%d bytes exceeds the maximum of %d", size, maximum)}
	}
	return Decision{Name: name, Allowed: true}
}

// BucketQuota refuses new uploads once the last-known bucket size passes the
// quota. The CloudWatch metric lags up to a day (M4b-3), so this is a
// backstop, not an exact meter.
func BucketQuota(currentBytes, incomingBytes, quotaBytes int64) Decision {
	name := "bucket_quota"
	if quotaBytes <= 0 {
		return Decision{Name: name, Allowed: true, Reason: "quota disabled"}
	}
	if currentBytes+incomingBytes > quotaBytes {
		return Decision{Name: name, Allowed: false,
			Reason: fmt.Sprintf("bucket holds ~%d bytes (metric lags ~1 day); adding %d would pass the %d quota — delete objects first", currentBytes, incomingBytes, quotaBytes)}
	}
	return Decision{Name: name, Allowed: true}
}
