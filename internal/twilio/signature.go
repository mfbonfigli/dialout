package twilio

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/url"
	"sort"
	"strings"
)

// Signature computes Twilio's X-Twilio-Signature for a request: base64(HMAC-SHA1(authToken,
// fullURL + every POST parameter name and value, sorted by name)). For a WebSocket handshake
// (a GET) params is empty and only the URL is signed.
func Signature(authToken, fullURL string, params url.Values) string {
	var b strings.Builder
	b.WriteString(fullURL)
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		vals := append([]string(nil), params[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			b.WriteString(k)
			b.WriteString(v)
		}
	}
	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(b.String()))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// ValidSignature checks a received X-Twilio-Signature in constant time. Twilio may sign the URL with
// or without the default port, so both forms are accepted (as Twilio's own SDKs do).
func ValidSignature(authToken, fullURL string, params url.Values, received string) bool {
	if authToken == "" || received == "" {
		return false
	}
	for _, u := range urlVariants(fullURL) {
		if hmac.Equal([]byte(Signature(authToken, u, params)), []byte(received)) {
			return true
		}
	}
	return false
}

func urlVariants(raw string) []string {
	out := []string{raw}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return out
	}
	def := map[string]string{"https": "443", "wss": "443", "http": "80", "ws": "80"}[u.Scheme]
	if def == "" {
		return out
	}
	alt := *u
	if u.Port() == "" {
		alt.Host = u.Host + ":" + def
	} else if u.Port() == def {
		alt.Host = u.Hostname()
	} else {
		return out
	}
	return append(out, alt.String())
}
