package twilio

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fakeTwilio struct {
	t        *testing.T
	calls    map[string]*Call
	requests []*http.Request
	forms    []url.Values
}

func (f *fakeTwilio) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != "SKkey" || pass != "secret" {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"code":20003,"message":"Authentication Error - No password provided","status":401}`)
		return
	}
	_ = r.ParseForm()
	f.requests = append(f.requests, r)
	f.forms = append(f.forms, r.PostForm)
	base := "/2010-04-01/Accounts/ACacct/Calls"
	switch {
	case r.Method == http.MethodPost && r.URL.Path == base+".json":
		c := &Call{SID: "CA1", Status: "queued", To: r.PostForm.Get("To"), From: r.PostForm.Get("From")}
		f.calls["CA1"] = c
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(c)
	case strings.HasPrefix(r.URL.Path, base+"/") && strings.HasSuffix(r.URL.Path, ".json"):
		sid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, base+"/"), ".json")
		c, ok := f.calls[sid]
		if !ok {
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"code":20404,"message":"not found","status":404}`)
			return
		}
		if r.Method == http.MethodPost {
			c.Status = r.PostForm.Get("Status")
		}
		_ = json.NewEncoder(w).Encode(c)
	default:
		w.WriteHeader(400)
	}
}

func TestClient(t *testing.T) {
	fake := &fakeTwilio{t: t, calls: map[string]*Call{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	c := &Client{AccountSID: "ACacct", KeySID: "SKkey", KeySecret: "secret", BaseURL: srv.URL}
	ctx := context.Background()

	call, err := c.CreateCall(ctx, CreateCallParams{
		To: "+393331234567", From: "+353871234567", TwiML: "<Response/>",
		Timeout: 25 * time.Second, TimeLimit: 315 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if call.SID != "CA1" || call.Status != "queued" {
		t.Fatalf("call = %+v", call)
	}
	form := fake.forms[0]
	for k, want := range map[string]string{"To": "+393331234567", "From": "+353871234567", "Twiml": "<Response/>", "Timeout": "25", "TimeLimit": "315"} {
		if form.Get(k) != want {
			t.Errorf("form %s = %q; want %q", k, form.Get(k), want)
		}
	}
	if form.Get("Record") != "" || form.Get("Url") != "" || form.Get("StatusCallback") != "" {
		t.Errorf("unexpected parameters: %v", form)
	}

	// Ringing call is canceled, answered call is completed, finished call is left alone.
	fake.calls["CA1"].Status = "ringing"
	if got, _ := c.EndCall(ctx, "CA1"); got.Status != "canceled" {
		t.Errorf("end ringing → %s", got.Status)
	}
	fake.calls["CA1"].Status = "in-progress"
	if got, _ := c.EndCall(ctx, "CA1"); got.Status != "completed" {
		t.Errorf("end in-progress → %s", got.Status)
	}
	n := len(fake.requests)
	if got, _ := c.EndCall(ctx, "CA1"); got.Status != "completed" || len(fake.requests) != n+1 {
		t.Errorf("end finished call should only fetch (requests %d → %d)", n, len(fake.requests))
	}

	_, err = c.FetchCall(ctx, "CAmissing")
	var te *Error
	if !errors.As(err, &te) || te.HTTPStatus != 404 || te.Code != 20404 {
		t.Errorf("missing call error = %v", err)
	}
	bad := &Client{AccountSID: "ACacct", KeySID: "SKkey", BaseURL: srv.URL}
	if _, err := bad.FetchCall(ctx, "CA1"); !errors.As(err, &te) || te.Code != 20003 {
		t.Errorf("auth error = %v", err)
	}
}

func TestSignature(t *testing.T) {
	// Reference computed independently with Python's hmac/hashlib for this input.
	params := url.Values{"CallSid": {"CA1234567890ABCDE"}, "Caller": {"+12349013030"}, "Digits": {"1234"}, "From": {"+12349013030"}, "To": {"+18005551212"}}
	u := "https://mycompany.com/myapp.php?foo=1&bar=2"
	got := Signature("12345", u, params)
	if got != refSignature {
		t.Fatalf("signature = %s; want %s", got, refSignature)
	}
	if !ValidSignature("12345", u, params, got) {
		t.Error("valid signature rejected")
	}
	if ValidSignature("12345", u, params, "AAAA"+got[4:]) || ValidSignature("other", u, params, got) || ValidSignature("12345", u, nil, got) {
		t.Error("invalid signature accepted")
	}
	if ValidSignature("", u, params, got) || ValidSignature("12345", u, params, "") {
		t.Error("empty token/signature accepted")
	}

	// WebSocket handshake: URL only, port variants accepted.
	ws := "wss://calls.example.com/twilio/stream/abc"
	sig := Signature("tok", "wss://calls.example.com:443/twilio/stream/abc", nil)
	if !ValidSignature("tok", ws, nil, sig) {
		t.Error("port variant rejected")
	}
	if ValidSignature("tok", "wss://calls.example.com/twilio/stream/abd", nil, sig) {
		t.Error("signature for another path accepted")
	}
}

func TestTokens(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		tok, err := NewStreamToken()
		if err != nil {
			t.Fatal(err)
		}
		if !ValidTokenFormat(tok) || seen[tok] {
			t.Fatalf("bad or repeated token %q", tok)
		}
		seen[tok] = true
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", 42), strings.Repeat("a", 44), strings.Repeat("a", 42) + "/", strings.Repeat("a", 42) + "="} {
		if ValidTokenFormat(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestStreamTwiML(t *testing.T) {
	x, err := StreamTwiML("wss://calls.example.com/twilio/stream/TOK", map[string]string{"call_id": `c"1<&>`, "a": "b"})
	if err != nil {
		t.Fatal(err)
	}
	want := `<?xml version="1.0" encoding="UTF-8"?><Response><Connect><Stream url="wss://calls.example.com/twilio/stream/TOK">` +
		`<Parameter name="a" value="b"/><Parameter name="call_id" value="c&#34;1&lt;&amp;&gt;"/></Stream></Connect></Response>`
	if x != want {
		t.Errorf("twiml =\n%s\nwant\n%s", x, want)
	}
	if _, err := StreamTwiML("https://x/y", nil); err == nil {
		t.Error("non-wss URL accepted")
	}
	if _, err := StreamTwiML("wss://x/y?a=1", nil); err == nil {
		t.Error("query string accepted")
	}
}

func TestStreamMessages(t *testing.T) {
	start := `{"event":"start","sequenceNumber":"1","start":{"accountSid":"ACacct","streamSid":"MZ1","callSid":"CA1","tracks":["inbound"],"mediaFormat":{"encoding":"audio/x-mulaw","sampleRate":8000,"channels":1},"customParameters":{"call_id":"c1"}},"streamSid":"MZ1"}`
	m, err := ParseInbound([]byte(start))
	if err != nil || m.Start.CallSID != "CA1" || m.Start.CustomParameters["call_id"] != "c1" {
		t.Fatalf("start = %+v, %v", m, err)
	}
	media := `{"event":"media","sequenceNumber":"3","media":{"track":"inbound","chunk":"1","timestamp":"5","payload":"` + base64.StdEncoding.EncodeToString([]byte{0xFF, 0x80}) + `"},"streamSid":"MZ1"}`
	m, err = ParseInbound([]byte(media))
	if err != nil {
		t.Fatal(err)
	}
	if ts, _ := m.Media.TimestampMillis(); ts != 5 {
		t.Errorf("timestamp = %d", ts)
	}
	if a, _ := m.Media.Audio(); len(a) != 2 || a[0] != 0xFF {
		t.Errorf("audio = %v", a)
	}
	for _, ok := range []string{
		`{"event":"connected","protocol":"Call","version":"1.0.0"}`,
		`{"event":"mark","sequenceNumber":"4","streamSid":"MZ1","mark":{"name":"m1"}}`,
		`{"event":"dtmf","streamSid":"MZ1","sequenceNumber":"5","dtmf":{"track":"inbound_track","digit":"1"}}`,
		`{"event":"stop","sequenceNumber":"6","streamSid":"MZ1","stop":{"accountSid":"ACacct","callSid":"CA1"}}`,
	} {
		if _, err := ParseInbound([]byte(ok)); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		`{"event":"start","start":{"callSid":"","streamSid":"MZ1"}}`,
		strings.Replace(start, "audio/x-mulaw", "audio/l16", 1),
		`{"event":"media"}`,
		`{"event":"mark"}`,
		`{"event":"bogus"}`,
		`not json`,
	} {
		if _, err := ParseInbound([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}

	var out map[string]any
	_ = json.Unmarshal(OutMedia("MZ1", []byte{0xFF}), &out)
	if out["event"] != "media" || out["streamSid"] != "MZ1" || out["media"].(map[string]any)["payload"] != "/w==" {
		t.Errorf("media out = %v", out)
	}
	_ = json.Unmarshal(OutMark("MZ1", "m7"), &out)
	if out["event"] != "mark" || out["mark"].(map[string]any)["name"] != "m7" {
		t.Errorf("mark out = %v", out)
	}
	if string(OutClear("MZ1")) != `{"event":"clear","streamSid":"MZ1"}` {
		t.Errorf("clear out = %s", OutClear("MZ1"))
	}
}

// refSignature was computed with Python's hmac and hashlib (independent implementation).
const refSignature = "0/KCTR6DLpKmkAf8muzZqo1nDgQ="
