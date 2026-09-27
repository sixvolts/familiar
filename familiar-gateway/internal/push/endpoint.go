package push

import (
	"fmt"
	"net/url"
	"strings"
)

// knownServices are the browser push services a subscription endpoint
// may point at (a host, or any subdomain of one).
var knownServices = []string{
	"fcm.googleapis.com",                // Chrome, Edge (Android), Chromium
	"android.googleapis.com",            // older Chrome endpoints
	"updates.push.services.mozilla.com", // Firefox
	"push.apple.com",                    // Safari (web.push.apple.com)
	"notify.windows.com",                // Edge on Windows (*.notify.windows.com)
}

// EndpointPolicy decides which subscription endpoints the gateway will
// POST to. A subscription's endpoint is a URL the browser hands us, so
// without a policy any user could register an internal address and
// have the gateway POST to it on every notification (a blind SSRF).
type EndpointPolicy struct {
	// ExtraHosts are exact hosts allowed besides the known services
	// ([push] allowed_endpoint_hosts), for a self-hosted push service.
	ExtraHosts []string
}

// Check returns an error when endpoint isn't an https URL on a known
// push service (or an extra host), on the default port, with no
// credentials in it.
func (p EndpointPolicy) Check(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("push endpoint must be an https URL")
	}
	if port := u.Port(); port != "" && port != "443" {
		return fmt.Errorf("push endpoint must use the default https port")
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range p.ExtraHosts {
		if host == strings.ToLower(strings.TrimSpace(h)) {
			return nil
		}
	}
	for _, svc := range knownServices {
		if host == svc || strings.HasSuffix(host, "."+svc) {
			return nil
		}
	}
	return fmt.Errorf("push endpoint host %q is not a known push service", host)
}
