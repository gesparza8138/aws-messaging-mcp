package guardrails

import (
	"strings"
	"testing"
	"time"
)

func TestContentTypeAllowed(t *testing.T) {
	for _, ok := range []string{"application/pdf", "image/png", "text/plain", " Video/MP4 "} {
		if d := ContentTypeAllowed(ok, false); !d.Allowed {
			t.Fatalf("%q rejected: %s", ok, d.Reason)
		}
	}
	for _, bad := range []string{"text/html", "IMAGE/SVG+XML", "application/x-msdownload", ""} {
		if d := ContentTypeAllowed(bad, false); d.Allowed {
			t.Fatalf("%q accepted", bad)
		}
	}
	if d := ContentTypeAllowed("text/html", true); !d.Allowed {
		t.Fatal("allowRisky must bypass the deny-list")
	}
	if d := ContentTypeAllowed("", true); d.Allowed {
		t.Fatal("empty type accepted even with allowRisky")
	}
}

func TestLinkExpiry(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	expiry, d := LinkExpiry("P3D", "", 365, now)
	if !d.Allowed || !expiry.Equal(now.Add(72*time.Hour)) {
		t.Fatalf("P3D: %v %+v", expiry, d)
	}
	expiry, d = LinkExpiry("P1DT6H30M", "", 365, now)
	if !d.Allowed || !expiry.Equal(now.Add(30*time.Hour+30*time.Minute)) {
		t.Fatalf("P1DT6H30M: %v %+v", expiry, d)
	}
	if _, d = LinkExpiry("P2W", "", 365, now); !d.Allowed {
		t.Fatalf("weeks: %+v", d)
	}
	expiry, d = LinkExpiry("", "2026-09-01T00:00:00Z", 365, now)
	if !d.Allowed || expiry.Day() != 1 {
		t.Fatalf("absolute: %v %+v", expiry, d)
	}
	cases := map[string][2]string{
		"both":     {"P3D", "2026-09-01T00:00:00Z"},
		"neither":  {"", ""},
		"garbage":  {"3 days", ""},
		"badtime":  {"", "tomorrow"},
		"past":     {"", "2020-01-01T00:00:00Z"},
		"toolong":  {"P400D", ""},
		"zero":     {"P0D", ""},
		"unitless": {"P3", ""},
		"shortp":   {"P", ""},
		"badtimeu": {"PT5X", ""},
		"timeless": {"PT5", ""},
		"hugenum":  {"P99999999999999999999D", ""},
	}
	for name, c := range cases {
		if _, d := LinkExpiry(c[0], c[1], 365, now); d.Allowed {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestSizeWithin(t *testing.T) {
	if d := SizeWithin("body", 100, 4<<20); !d.Allowed {
		t.Fatalf("small body rejected: %s", d.Reason)
	}
	if d := SizeWithin("body", 4<<20+1, 4<<20); d.Allowed || !strings.Contains(d.Name, "body") {
		t.Fatalf("oversize accepted: %+v", d)
	}
	if d := SizeWithin("upload", 0, 10); d.Allowed {
		t.Fatal("zero size accepted")
	}
}

func TestBucketQuota(t *testing.T) {
	if d := BucketQuota(4<<30, 1<<20, 5<<30); !d.Allowed {
		t.Fatalf("under quota rejected: %s", d.Reason)
	}
	if d := BucketQuota(5<<30, 1, 5<<30); d.Allowed {
		t.Fatal("over quota accepted")
	}
	if d := BucketQuota(100, 1, 0); !d.Allowed {
		t.Fatal("disabled quota must allow")
	}
}
