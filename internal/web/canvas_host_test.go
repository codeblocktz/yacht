package web

import (
	"strings"
	"testing"
)

// Somebody who brought their own domain wants their service shown at it. The
// canvas card, its panel and the apps list each showed the platform hostname
// instead, whatever was attached; each now shows the live custom domain where
// there is one and the platform hostname otherwise.
//
// Whether a domain is live is the store's to say — Host and CustomHost arrive
// here already decided — so this checks only which of the two is shown.
func TestServiceIsShownAtItsCustomDomain(t *testing.T) {
	web := sampleApp("owner-1", "web")
	web.Host, web.CustomHost = "web-production.up.example.com", "www.customer.test"
	api := sampleApp("owner-1", "api")
	api.Host = "api-production.up.example.com"
	h := testServer(t, Options{Apps: newFakeApps(web, api)})

	canvas := get(t, h, "/projects/default").Body.String()
	for _, want := range []string{">www.customer.test</span>", ">api-production.up.example.com</span>"} {
		if !strings.Contains(canvas, want) {
			t.Errorf("the canvas does not show %s", want)
		}
	}
	if strings.Contains(canvas, "web-production.up.example.com") {
		t.Error("the canvas shows web at its platform hostname, though it has a custom domain")
	}

	panel := get(t, h, "/apps/web").Body.String()
	if !strings.Contains(panel, `href="https://www.customer.test"`) {
		t.Error("web's panel does not link to its custom domain over https")
	}
	if strings.Contains(panel, "web-production.up.example.com") {
		t.Error("web's panel shows its platform hostname, though it has a custom domain")
	}
	if other := get(t, h, "/apps/api").Body.String(); !strings.Contains(other, `href="http://api-production.up.example.com"`) {
		t.Error("api's panel does not link to its platform hostname")
	}

	list := get(t, h, "/apps").Body.String()
	for _, want := range []string{"www.customer.test", "api-production.up.example.com"} {
		if !strings.Contains(list, want) {
			t.Errorf("the apps list does not show %s", want)
		}
	}
}
