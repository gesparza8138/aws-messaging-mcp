package guardrails

import (
	"strings"
	"testing"
)

func TestDestinationCountryUS(t *testing.T) {
	for _, ok := range []string{"+12065550100", "+19995550199", " +12065550100 "} {
		if d := DestinationCountryUS(ok); !d.Allowed {
			t.Fatalf("%q rejected: %s", ok, d.Reason)
		}
	}
	for _, bad := range []string{"", "+442071234567", "+11065550100", "+1206555010", "+120655501000", "12065550100", "+1206555010a"} {
		if d := DestinationCountryUS(bad); d.Allowed {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestMaxPriceCapped(t *testing.T) {
	if v, d := MaxPriceCapped("", "0.05"); v != "0.05" || !d.Allowed {
		t.Fatalf("default: %q %+v", v, d)
	}
	if v, d := MaxPriceCapped("0.03", "0.05"); v != "0.03" || !d.Allowed || d.Reason != "" {
		t.Fatalf("under ceiling: %q %+v", v, d)
	}
	if v, d := MaxPriceCapped("0.50", "0.05"); v != "0.05" || !d.Allowed || !strings.Contains(d.Reason, "capped") {
		t.Fatalf("capped: %q %+v", v, d)
	}
	for _, bad := range []string{"free", "-1", "0"} {
		if _, d := MaxPriceCapped(bad, "0.05"); d.Allowed {
			t.Fatalf("%q accepted", bad)
		}
	}
	if _, d := MaxPriceCapped("0.03", "broken"); d.Allowed {
		t.Fatal("misconfigured ceiling accepted")
	}
}

func TestOriginationAllowed(t *testing.T) {
	if d := OriginationAllowed("", "+18885550100"); !d.Allowed {
		t.Fatalf("default origination rejected: %s", d.Reason)
	}
	if d := OriginationAllowed("+18885550100", "+18885550100"); !d.Allowed {
		t.Fatalf("configured origination rejected: %s", d.Reason)
	}
	if d := OriginationAllowed("+15550001111", "+18885550100"); d.Allowed {
		t.Fatal("foreign origination accepted")
	}
}

func TestMediaURLsInBucket(t *testing.T) {
	bucket := "media-bucket"
	if d := MediaURLsInBucket(nil, bucket); !d.Allowed {
		t.Fatalf("no media rejected: %s", d.Reason)
	}
	if d := MediaURLsInBucket([]string{"s3://media-bucket/a.jpg"}, bucket); !d.Allowed {
		t.Fatalf("in-bucket URL rejected: %s", d.Reason)
	}
	for _, bad := range [][]string{
		{"s3://other-bucket/a.jpg"},
		{"https://media-bucket/a.jpg"},
		{"s3://media-bucket/"},
		{"s3://media-bucket/a.jpg", "s3://elsewhere/b.jpg"},
	} {
		if d := MediaURLsInBucket(bad, bucket); d.Allowed {
			t.Fatalf("%v accepted", bad)
		}
	}
	if d := MediaURLsInBucket([]string{"s3:///a.jpg"}, ""); d.Allowed {
		t.Fatal("unconfigured bucket accepted media")
	}
}

func TestMediaUploadAllowed(t *testing.T) {
	if d := MediaUploadAllowed("image/jpeg", 1024, 5<<20); !d.Allowed {
		t.Fatalf("jpeg rejected: %s", d.Reason)
	}
	if d := MediaUploadAllowed(" IMAGE/PNG ", 1, 5<<20); !d.Allowed {
		t.Fatalf("case/space handling: %s", d.Reason)
	}
	if d := MediaUploadAllowed("text/html", 1024, 5<<20); d.Allowed {
		t.Fatal("html accepted")
	}
	if d := MediaUploadAllowed("image/gif", 5<<20+1, 5<<20); d.Allowed {
		t.Fatal("oversize accepted")
	}
	if d := MediaUploadAllowed("image/gif", 0, 5<<20); d.Allowed {
		t.Fatal("empty accepted")
	}
}
