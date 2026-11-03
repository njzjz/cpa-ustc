package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cpa-ustc/internal/ustc"
)

const donationPublicPrefix = "/api/donation/"

// NewDonationGateway serves a public donation site without exposing CPA's
// management API. The operator's key stays in this server and is sent only to
// four fixed authenticated donation operations, never to a browser or redirect.
func NewDonationGateway(backend, origin, key string) (http.Handler, error) {
	config := ustc.DefaultConfig()
	config.DonationMode, config.DonationOrigin = "open", origin
	if err := config.Validate(); err != nil {
		return nil, err
	}
	origin = strings.TrimSuffix(origin, "/")
	target, err := url.Parse(backend)
	if err != nil || target.Hostname() == "" || target.User != nil || target.Opaque != "" || target.RawQuery != "" || target.ForceQuery || target.RawPath != "" || target.Fragment != "" || (target.Path != "" && target.Path != "/") || (target.Scheme != "https" && !(target.Scheme == "http" && (target.Hostname() == "localhost" || target.Hostname() == "127.0.0.1" || target.Hostname() == "::1"))) {
		return nil, errors.New("CPA 地址必须是无路径的 HTTPS 源；仅本机允许 HTTP")
	}
	if key == "" || strings.TrimSpace(key) != key || strings.ContainsAny(key, "\r\n\x00") {
		return nil, errors.New("CPA 管理密钥为空或格式无效")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &donationGateway{backend: strings.TrimSuffix(backend, "/"), origin: origin, key: key, client: client}, nil
}

type donationGateway struct {
	backend string
	origin  string
	key     string
	client  *http.Client
}

func writeDonationResponse(writer http.ResponseWriter, response managementResponse) {
	for name, values := range response.Headers {
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(response.Body)
}

func (gateway *donationGateway) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	respond := func(response managementResponse) { writeDonationResponse(writer, response) }
	if request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.EscapedPath() != request.URL.Path {
		respond(donationError(404, "接口不存在"))
		return
	}
	if request.Method == http.MethodGet {
		name, contentType := "", ""
		switch request.URL.Path {
		case "/":
			name, contentType = "donate.html", "text/html; charset=utf-8"
		case "/donate.js":
			name, contentType = "donate.js", "application/javascript; charset=utf-8"
		case "/settings.css":
			name, contentType = "settings.css", "text/css; charset=utf-8"
		default:
			respond(donationError(404, "接口不存在"))
			return
		}
		data, _ := webAssets.ReadFile("web/" + name)
		response := webResponse(200, contentType, data)
		response.Headers.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		response.Headers.Set("Cross-Origin-Opener-Policy", "same-origin")
		respond(response)
		return
	}
	operation := strings.TrimPrefix(request.URL.Path, donationPublicPrefix)
	if request.Method != http.MethodPost || !strings.HasPrefix(request.URL.Path, donationPublicPrefix) || (operation != "session" && operation != "start" && operation != "poll" && operation != "confirm") {
		respond(donationError(404, "接口不存在"))
		return
	}
	if request.Header.Get("Sec-Fetch-Site") != "same-origin" || request.Header.Get("X-USTC-Donation-Origin") != gateway.origin || (request.Header.Get("Origin") != "" && request.Header.Get("Origin") != gateway.origin) || (request.Header.Get("Sec-Fetch-Mode") != "same-origin" && request.Header.Get("Sec-Fetch-Mode") != "cors") {
		respond(donationError(403, "仅允许从本站捐献页面发起请求"))
		return
	}
	// Operations use a fixed header schema, not arbitrary bodies or target URLs.
	body, err := io.ReadAll(io.LimitReader(request.Body, 1))
	if err != nil || len(body) != 0 {
		respond(donationError(400, "捐献请求不接受请求体"))
		return
	}
	upstream, err := http.NewRequestWithContext(request.Context(), http.MethodPost, gateway.backend+donationManagementPrefix+operation, nil)
	if err != nil {
		respond(donationError(502, "捐献网关配置无效"))
		return
	}
	// Never forward client Authorization, proxy headers, or management cookies.
	// Browser origin metadata is preserved, not manufactured by this gateway.
	for _, name := range []string{"Origin", "Sec-Fetch-Site", "Sec-Fetch-Mode", "X-USTC-Donation-Origin", "X-USTC-Donation-CSRF", "X-USTC-Donation-Action", "X-USTC-Donation-Consent", "X-USTC-Donation-Confirm"} {
		for _, value := range request.Header.Values(name) {
			upstream.Header.Add(name, value)
		}
	}
	for _, cookie := range request.Cookies() {
		if cookie.Name == "__Secure-ustc_donation" || cookie.Name == "ustc_donation_dev" {
			upstream.AddCookie(cookie)
		}
	}
	upstream.Header.Set("Authorization", "Bearer "+gateway.key)
	response, err := gateway.client.Do(upstream)
	if err != nil {
		respond(donationError(502, "暂时无法连接 CPA；请联系本站管理员"))
		return
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	// Suppress host authentication errors, redirects and non-JSON responses:
	// they contain deployment details, not donor-facing operation results.
	if err != nil || len(data) > 64<<10 || response.StatusCode < 200 || response.StatusCode >= 500 || response.StatusCode == 401 || response.StatusCode == 404 || response.StatusCode >= 300 && response.StatusCode < 400 || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") || !jsonDonationObject(data) {
		respond(donationError(502, "CPA 捐献服务不可用；请管理员检查管理密钥、插件版本及设置"))
		return
	}
	result := donationJSON(response.StatusCode, nil)
	result.Body = data
	for _, cookie := range response.Cookies() {
		if (cookie.Name == "__Secure-ustc_donation" || cookie.Name == "ustc_donation_dev") && cookie.Domain == "" && cookie.Path == donationPublicPrefix && cookie.HttpOnly && cookie.SameSite == http.SameSiteStrictMode {
			result.Headers.Add("Set-Cookie", cookie.String())
		}
	}
	respond(result)
}

func jsonDonationObject(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}
