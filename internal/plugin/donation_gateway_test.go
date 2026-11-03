package plugin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDonationGatewayKeepsConsentAndConfirmationInsideCPA(t *testing.T) {
	runtime, store := donatedRuntime(t)
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer server-only-key" {
			writer.WriteHeader(401)
			return
		}
		service, ctx, _ := runtime.current()
		writeDonationResponse(writer, runtime.handleManagement(ctx, service, rpcRequest{Method: request.Method, Path: request.URL.Path, Headers: request.Header, HostCallbackID: "donation-scope"}))
	}))
	defer backend.Close()
	gateway, err := NewDonationGateway(backend.URL, "https://cpa.example", "server-only-key")
	if err != nil {
		t.Fatal(err)
	}
	runtime.donations.origin = "https://cpa.example"
	csrf, confirmation, cookie := "", "", ""
	request := func(operation string, extra http.Header, status int) map[string]any {
		t.Helper()
		input := httptest.NewRequest("POST", donationPublicPrefix+operation, nil)
		input.Header = http.Header{"Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"same-origin"}, "Origin": {"https://cpa.example"}, "X-Ustc-Donation-Origin": {"https://cpa.example"}, "X-Ustc-Donation-Csrf": {csrf}, "Cookie": {cookie}}
		for name, values := range extra {
			input.Header[name] = values
		}
		output := httptest.NewRecorder()
		gateway.ServeHTTP(output, input)
		if output.Code != status {
			t.Fatalf("%s returned %d: %s", operation, output.Code, output.Body.String())
		}
		if cookies := output.Result().Cookies(); len(cookies) > 0 {
			cookie = cookies[0].Name + "=" + cookies[0].Value
		}
		var body map[string]any
		if err := json.Unmarshal(output.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	csrf = request("session", nil, 200)["csrf"].(string)
	request("start", http.Header{"X-Ustc-Donation-Action": {"donate"}}, 400)
	request("start", http.Header{"X-Ustc-Donation-Action": {"donate"}, "X-Ustc-Donation-Consent": {"share"}}, 200)
	confirmation = request("poll", nil, 200)["confirmation"].(string)
	request("confirm", nil, 409)
	if store.writes != 0 {
		t.Fatal("school login implicitly modified the CPA pool")
	}
	request("confirm", http.Header{"X-Ustc-Donation-Confirm": {confirmation}}, 200)
	request("confirm", http.Header{"X-Ustc-Donation-Confirm": {confirmation}}, 409)
	if store.writes != 1 {
		t.Fatal("single-use confirmation did not save exactly once")
	}
	request("start", http.Header{"X-Ustc-Donation-Action": {"withdraw"}}, 200)
	confirmation = request("poll", nil, 200)["confirmation"].(string)
	request("confirm", http.Header{"X-Ustc-Donation-Confirm": {confirmation}}, 200)
	if store.writes != 2 || strings.Contains(string(store.files["ustc-donation-donor.json"]), "private-secret") || !strings.Contains(string(store.files["ustc-donation-donor.json"]), `"donation_revoked":true`) {
		t.Fatal("gateway withdrawal did not clear native credential tokens")
	}
}

func TestDonationGatewayRestrictedForwarding(t *testing.T) {
	runtime, store := donatedRuntime(t)
	calls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.Method != "POST" || request.URL.Path != donationManagementPrefix+"session" || request.Header.Get("Authorization") != "Bearer server-only-key" {
			t.Error("gateway forwarded an unexpected management request")
		}
		if request.Header.Get("X-Forwarded-Host") != "" || strings.Contains(request.Header.Get("Cookie"), "management") {
			t.Error("gateway forwarded untrusted management/proxy headers")
		}
		service, ctx, _ := runtime.current()
		writeDonationResponse(writer, runtime.handleManagement(ctx, service, rpcRequest{Method: request.Method, Path: request.URL.Path, Headers: request.Header}))
	}))
	defer backend.Close()
	gateway, err := NewDonationGateway(backend.URL, "https://cpa.example", "server-only-key")
	if err != nil {
		t.Fatal(err)
	}
	// This origin must match the portal fixture, not the CPA backend address.
	runtime.donations.origin = "https://cpa.example"
	request := httptest.NewRequest("POST", donationPublicPrefix+"session", nil)
	request.Header = http.Header{"Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"same-origin"}, "Origin": {"https://cpa.example"}, "X-Ustc-Donation-Origin": {"https://cpa.example"}, "Authorization": {"Bearer attacker-key"}, "Cookie": {"management=private"}, "X-Forwarded-Host": {"evil.example"}}
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"csrf"`) || len(response.Result().Cookies()) != 1 || response.Result().Cookies()[0].Path != donationPublicPrefix || strings.Contains(response.Body.String(), "server-only-key") {
		t.Fatal("anonymous session bootstrap failed", response.Code, response.Body.String())
	}
	for _, path := range []string{"/v8/management/credentials", managementPrefix + "accounts", resourcePrefix + "donate/confirm", donationPublicPrefix + "../confirm", donationPublicPrefix + "confirm?target=/accounts", "/api/donation/%63onfirm", donationPublicPrefix + "confirm/", "/unknown"} {
		response = httptest.NewRecorder()
		gateway.ServeHTTP(response, httptest.NewRequest("POST", path, nil))
		if response.Code != 404 {
			t.Error("non-allowlisted path accepted", path, response.Code)
		}
	}
	for _, method := range []string{"GET", "HEAD", "OPTIONS", "PUT", "DELETE"} {
		response = httptest.NewRecorder()
		gateway.ServeHTTP(response, httptest.NewRequest(method, donationPublicPrefix+"session", nil))
		if response.Code != 404 {
			t.Error("non-POST operation accepted", method)
		}
	}
	for _, header := range []string{"Sec-Fetch-Site", "Origin", "X-USTC-Donation-Origin", "Sec-Fetch-Mode"} {
		invalid := request.Clone(request.Context())
		invalid.Header.Set(header, "https://evil.example")
		response = httptest.NewRecorder()
		gateway.ServeHTTP(response, invalid)
		if response.Code != 403 {
			t.Error("cross-origin request accepted", header)
		}
	}
	invalid := request.Clone(request.Context())
	invalid.Body = io.NopCloser(strings.NewReader(`{"target":"/accounts"}`))
	response = httptest.NewRecorder()
	gateway.ServeHTTP(response, invalid)
	if response.Code != 400 || calls != 1 || store.calls != 0 {
		t.Fatal("invalid input reached CPA or bootstrap invoked a credential callback", response.Code, calls, store.calls)
	}
	for _, path := range []string{"/", "/donate.js", "/settings.css"} {
		response = httptest.NewRecorder()
		gateway.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 200 || strings.Contains(response.Body.String(), "server-only-key") || response.Header().Get("Cache-Control") != "no-store" {
			t.Error("public asset missing or unsafe", path)
		}
	}
}

func TestDonationGatewayRejectsUnsafeBackendsAndResponses(t *testing.T) {
	for _, backend := range []string{"http://remote.example", "https://user:pass@example.com", "https://example.com/path", "https://example.com?target=x", "https://example.com#x", "https://example.com?"} {
		if _, err := NewDonationGateway(backend, "https://donate.example", "private"); err == nil {
			t.Error("unsafe backend accepted", backend)
		}
	}
	for _, origin := range []string{"", "http://donate.example", "https://donate.example/path"} {
		if _, err := NewDonationGateway("http://127.0.0.1:8317", origin, "private"); err == nil {
			t.Error("unsafe origin accepted", origin)
		}
	}
	for _, key := range []string{"", " bad", "bad\r\ninjected: yes"} {
		if _, err := NewDonationGateway("http://127.0.0.1:8317", "https://donate.example", key); err == nil {
			t.Error("unsafe key accepted")
		}
	}
	redirected := false
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer sink.Close()
	for _, status := range []int{302, 401, 404, 500, 200} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Location", sink.URL)
				writer.Header().Set("Content-Type", "text/html")
				writer.WriteHeader(status)
				_, _ = writer.Write([]byte("private backend details"))
			}))
			defer backend.Close()
			gateway, err := NewDonationGateway(backend.URL, "https://donate.example", "private")
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", donationPublicPrefix+"session", nil)
			request.Header = http.Header{"Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"same-origin"}, "X-Ustc-Donation-Origin": {"https://donate.example"}}
			response := httptest.NewRecorder()
			gateway.ServeHTTP(response, request)
			if response.Code != 502 || redirected || strings.Contains(response.Body.String(), "private") || response.Header().Get("Location") != "" {
				t.Error("unsafe backend response or redirect exposed", response.Code)
			}
		})
	}
}

func TestManagementRegistrationHasPassiveResourcesOnly(t *testing.T) {
	registration := managementRegistration().(map[string]any)
	resources := registration["resources"].([]map[string]string)
	if len(resources) != 3 {
		t.Fatal("unexpected resource registration", resources)
	}
	for _, resource := range resources {
		if resource["Path"] != "/settings" && resource["Path"] != "/settings.js" && resource["Path"] != "/settings.css" {
			t.Fatal("active resource registered", resource)
		}
	}
	operations := 0
	for _, route := range registration["routes"].([]map[string]string) {
		if strings.HasPrefix(route["Path"], donationManagementPrefix) {
			operations++
			if route["Method"] != "POST" {
				t.Fatal("stateful donation operation is not POST", route)
			}
		}
	}
	if operations != 4 {
		t.Fatal("donation management operations missing")
	}
}
