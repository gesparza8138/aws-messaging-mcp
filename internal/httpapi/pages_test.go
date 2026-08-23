package httpapi

import (
	"strings"
	"testing"
)

func TestOptInPageNumber(t *testing.T) {
	if page := optInPage("+18885550100"); !strings.Contains(page, "+18885550100") {
		t.Fatal("configured number missing from opt-in page")
	}
	if page := optInPage(""); !strings.Contains(page, "our toll-free number") {
		t.Fatal("fallback phrase missing from opt-in page")
	}
}
