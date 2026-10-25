package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sample = `
owner: Marco
timezone: Europe/Rome
twilio:
  account_sid: ${T_SID}
  api_key_sid: SK123
  api_key_secret: ${T_SECRET}
  auth_token: ${T_TOKEN}
  from: "+353 85 000 0000"
engine:
  name: sonic
prompt:
  file: prompt.tmpl
storage:
  record_audio: true
  spool: data/spool
policy:
  enabled: true
  allowlist:
    - number: "+39 333 000 0000"
      label: Marco (mobile)
      name: Marco
      kind: personal
    - number: "02 1234 5678"
      label: Pizzeria
  limits:
    per_hour: 0
`

func write(t *testing.T, dir, name, text string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("T_SID", "AC123")
	t.Setenv("T_SECRET", "s#cret: with yaml")
	write(t, dir, "secrets.env", "T_TOKEN='tok$en'\nT_SID=from-file\n")
	write(t, dir, "prompt.tmpl", `{{define "system"}}Call for {{.Owner}}: {{.Objective}} {{.Closing}}{{end}}`)
	path := write(t, dir, "dialout.yaml", "secrets_file: secrets.env\n"+sample)

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// Secrets: from the environment, which wins over the secrets file, and from the file.
	if c.Twilio.AccountSID != "AC123" || c.Twilio.APIKeySecret != "s#cret: with yaml" || c.Twilio.AuthToken != "tok$en" {
		t.Errorf("twilio = %+v", c.Twilio)
	}
	// Defaults, and relative paths from the file's folder.
	if c.Engine.Sonic.Voice != "beatrice" || c.Engine.Sonic.Region != "eu-north-1" || c.Tunnel.Listen != "127.0.0.1:8090" ||
		c.Storage.SegmentSeconds != 60 || c.Server.MCPListen != ":8080" || c.Engine.Language != "it" {
		t.Errorf("defaults: %+v", c)
	}
	if c.Storage.Spool != filepath.Join(dir, "data", "spool") || !strings.HasSuffix(c.Storage.Index, "calls.json") ||
		c.Prompt.File != filepath.Join(dir, "prompt.tmpl") {
		t.Errorf("paths: spool %s, index %s, prompt %s", c.Storage.Spool, c.Storage.Index, c.Prompt.File)
	}
	if c.Location.String() != "Europe/Rome" || c.Prompts.Source != c.Prompt.File || c.Path != path {
		t.Errorf("location %v, prompts %q", c.Location, c.Prompts.Source)
	}
	p := c.PolicyConfig()
	if !p.Enabled || p.Mode != "allowlist" || len(p.Allowlist) != 2 || p.Allowlist[0].Number != "+393330000000" ||
		p.Allowlist[0].Kind != "personal" || p.Allowlist[1].Number != "+390212345678" ||
		p.MaxPerHour != 0 || p.MaxPerDay != 20 || p.MaxCallSeconds != 300 || p.MaxCallee != 100 {
		t.Errorf("policy = %+v", p)
	}
	// The dialling defaults: Italy, fixed and mobile; for open mode, 09:00-21:00 and 2 a day.
	if strings.Join(p.Dialable.Countries, ",") != "IT" || p.Dialable.DefaultCountry != "IT" || len(p.Dialable.Kinds) != 2 ||
		p.Hours.String() != "09:00-21:00" || p.MaxPerNumberPerDay != 2 || p.Location != c.Location || len(p.Blocklist) != 0 {
		t.Errorf("dialling policy = %+v", p)
	}
	if err := c.CheckServer(); err == nil {
		t.Error("server without a token accepted")
	}
	c.Server.MCPToken = strings.Repeat("a", 32)
	if err := c.CheckServer(); err != nil {
		t.Error(err)
	}
}

func TestErrors(t *testing.T) {
	t.Setenv("T_SID", "AC123")
	t.Setenv("T_SECRET", "x")
	t.Setenv("T_TOKEN", "x")
	base := strings.Replace(sample, "prompt:\n  file: prompt.tmpl\n", "", 1)
	if _, err := Parse([]byte(base), t.TempDir()); err != nil {
		t.Fatalf("base config: %v", err)
	}
	for name, c := range map[string]struct{ text, want string }{
		"unknown key":           {base + "polcy: {}\n", "polcy"},
		"typo in limit":         {strings.Replace(base, "per_hour: 0", "per_hour: 0\n    per_minute: 1", 1), "per_minute"},
		"wrong type":            {strings.Replace(base, "record_audio: true", "record_audio: maybe", 1), "into bool"},
		"empty":                 {"", "empty"},
		"two documents":         {base + "---\nowner: x\n", "more than one"},
		"unset secret":          {strings.Replace(base, "${T_TOKEN}", "${NOT_SET_ANYWHERE}", 1), "twilio.auth_token is empty (NOT_SET_ANYWHERE not set)"},
		"no owner":              {strings.Replace(base, "owner: Marco", "owner: \"\"", 1), "owner"},
		"engine":                {strings.Replace(base, "name: sonic", "name: watson", 1), "engine.name"},
		"elevenlabs":            {strings.Replace(base, "name: sonic", "name: elevenlabs", 1), "engine.elevenlabs.api_key is empty"},
		"tunnel token":          {base + "tunnel:\n  hostname: calls.example.com\n", "tunnel.token is empty"},
		"bad number":            {strings.Replace(base, "02 1234 5678", "112", 1), "policy.allowlist"},
		"bad kind":              {strings.Replace(base, "kind: personal", "kind: friend", 1), "kind"},
		"long call":             {strings.Replace(base, "per_hour: 0", "max_minutes: 8", 1), "max_minutes"},
		"ssm":                   {strings.Replace(base, "enabled: true", "enabled: true\n  source: ssm", 1), "policy.source"},
		"timezone":              {strings.Replace(base, "Europe/Rome", "Europe/Atlantis", 1), "timezone"},
		"prompt file":           {base + "prompt:\n  file: missing.tmpl\n", "prompt file"},
		"mode":                  {strings.Replace(base, "enabled: true", "enabled: true\n  mode: everyone", 1), "policy.mode"},
		"country":               {strings.Replace(base, "enabled: true", "enabled: true\n  countries: [IT, XX]", 1), "XX"},
		"no countries":          {strings.Replace(base, "enabled: true", "enabled: true\n  countries: []", 1), "policy.countries is empty"},
		"default":               {strings.Replace(base, "enabled: true", "enabled: true\n  default_country: Italy", 1), "default_country"},
		"number type":           {strings.Replace(base, "enabled: true", "enabled: true\n  number_types: [mobile, premium]", 1), "premium"},
		"blocklist":             {strings.Replace(base, "enabled: true", "enabled: true\n  blocklist: [\"02 1234\"]", 1), "policy.blocklist"},
		"hours":                 {strings.Replace(base, "enabled: true", "enabled: true\n  hours: 9-21", 1), "policy.hours"},
		"listed abroad":         {strings.Replace(base, "enabled: true", "enabled: true\n  countries: [IE]", 1), "country not allowed: IT"},
		"audio without storage": {strings.Replace(base, "record_audio: true", "record_audio: true\n  enabled: false", 1), "storage.record_audio needs storage.enabled"},
		"watchdog key":          {base + "watchdog:\n  enabled: true\n", "watchdog.api_key is empty"},
		"watchdog threshold":    {base + "watchdog:\n  enabled: true\n  api_key: k\n  thresholds: {hang_up: 1.5}\n", "watchdog.thresholds.hang_up"},
		"watchdog every":        {base + "watchdog:\n  enabled: true\n  api_key: k\n  check_every: 0\n", "watchdog.check_every"},
		"watchdog removed key":  {base + "watchdog:\n  enabled: true\n  api_key: k\n  thresholds: {wrap_up: 0.8}\n", "wrap_up"},
		"watchdog timeout":      {base + "watchdog:\n  enabled: true\n  api_key: k\n  timeout: 1m\n", "watchdog.timeout"},
		"per number":            {strings.Replace(base, "per_hour: 0", "per_number_per_day: -1", 1), "per_number_per_day"},
		"duplicate key":         {base + "watchdog:\n  enabled: false\nwatchdog:\n  enabled: true\n", "already defined"},
		"s3 region, no bucket":  {strings.Replace(base, "record_audio: true", "record_audio: true\n  s3_region: eu-west-1", 1), "need storage.s3_bucket"},
		"s3 prefix, no bucket":  {strings.Replace(base, "record_audio: true", "record_audio: true\n  s3_prefix: dialout", 1), "need storage.s3_bucket"},
		"s3 absolute prefix":    {strings.Replace(base, "record_audio: true", "record_audio: true\n  s3_bucket: b\n  s3_prefix: /dialout", 1), "storage.s3_prefix"},
		"s3 prefix with ..":     {strings.Replace(base, "record_audio: true", "record_audio: true\n  s3_bucket: b\n  s3_prefix: a/../b", 1), "storage.s3_prefix"},
	} {
		_, err := Parse([]byte(c.text), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want it to mention %q", name, err, c.want)
		}
	}
}

func TestS3Storage(t *testing.T) {
	t.Setenv("T_SID", "AC123")
	t.Setenv("T_SECRET", "x")
	t.Setenv("T_TOKEN", "x")
	text := strings.Replace(sample, "prompt:\n  file: prompt.tmpl\n", "", 1)
	text = strings.Replace(text, "record_audio: true", "record_audio: true\n  s3_bucket: my-calls\n  s3_region: eu-south-1\n  s3_prefix: hermes/dialout", 1)
	c, err := Parse([]byte(text), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s := c.Storage; s.S3Bucket != "my-calls" || s.S3Region != "eu-south-1" || s.S3Prefix != "hermes/dialout/" {
		t.Errorf("storage = %+v", s)
	}
}

func TestOpenPolicy(t *testing.T) {
	t.Setenv("T_SID", "AC123")
	t.Setenv("T_SECRET", "x")
	t.Setenv("T_TOKEN", "x")
	text := strings.Replace(sample, "prompt:\n  file: prompt.tmpl\n", "", 1)
	text = strings.Replace(text, "enabled: true", `enabled: true
  mode: open
  countries: [it, IE]
  number_types: [fixed, mobile, toll_free]
  blocklist: ["+39 02 1234 5678", "+353 1"]
  hours: "08:00-20:00"`, 1)
	text = strings.Replace(text, "per_hour: 0", "per_hour: 0\n    per_number_per_day: 1", 1)
	text = strings.Replace(text, "02 1234 5678\"\n      label: Pizzeria", "+353 85 123 4567\"\n      label: Dublin", 1)
	c, err := Parse([]byte(text), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := c.PolicyConfig()
	if p.Mode != "open" || strings.Join(p.Dialable.Countries, ",") != "IT,IE" || len(p.Dialable.Kinds) != 3 ||
		strings.Join(p.Blocklist, ",") != "+390212345678,+3531" || p.Hours.String() != "08:00-20:00" || p.MaxPerNumberPerDay != 1 ||
		p.Allowlist[1].Number != "+353851234567" {
		t.Errorf("open policy = %+v", p)
	}
}

func TestWatchdogConfig(t *testing.T) {
	t.Setenv("T_SID", "AC123")
	t.Setenv("T_SECRET", "x")
	t.Setenv("T_TOKEN", "x")
	t.Setenv("OR_KEY", "or-key")
	text := strings.Replace(sample, "prompt:\n  file: prompt.tmpl\n", "", 1) +
		"watchdog:\n  enabled: true\n  api_key: ${OR_KEY}\n  timeout: 2500ms\n  check_every: 6\n  thresholds: {hang_up: 0.95}\n"
	c, err := Parse([]byte(text), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := c.Watchdog
	if !w.Enabled || w.APIKey != "or-key" || w.Model != "openai/gpt-6-luna-decisions" || w.BaseURL != "https://openrouter.ai/api" ||
		w.Timeout != 2500*time.Millisecond || w.CheckEvery != 6 || w.Thresholds.HangUp != 0.95 {
		t.Errorf("watchdog = %+v", w)
	}
}

func TestExpandString(t *testing.T) {
	t.Setenv("A_VAR", "value")
	for in, want := range map[string]string{
		"plain":              "plain",
		"${A_VAR}":           "value",
		"x-${A_VAR}-y":       "x-value-y",
		"$$5 and $$${A_VAR}": "$5 and $value",
		"$5 and ${":          "$5 and ${",
		"${not valid}":       "${not valid}",
		"trailing $":         "trailing $",
	} {
		if got, _ := expandString(in); got != want {
			t.Errorf("expandString(%q) = %q, want %q", in, got, want)
		}
	}
	if _, missing := expandString("${NOPE_1}${A_VAR}${NOPE_2}"); strings.Join(missing, ",") != "NOPE_1,NOPE_2" {
		t.Errorf("missing = %v", missing)
	}
}

func TestFind(t *testing.T) {
	if p, err := Find("x.yaml"); err != nil || p != "x.yaml" {
		t.Errorf("Find(flag) = %q, %v", p, err)
	}
	t.Setenv(Env, "from-env.yaml")
	if p, err := Find(""); err != nil || p != "from-env.yaml" {
		t.Errorf("Find(env) = %q, %v", p, err)
	}
}

func TestExampleConfig(t *testing.T) {
	b, err := os.ReadFile("../../examples/dialout.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"TWILIO_ACCOUNT_SID", "TWILIO_API_KEY_SID", "TWILIO_API_KEY_SECRET", "TWILIO_AUTH_TOKEN"} {
		t.Setenv(k, "x")
	}
	t.Setenv("DIALOUT_MCP_TOKEN", strings.Repeat("t", 64))
	t.Setenv(DataEnv, "/data")
	text := strings.Replace(string(b), "secrets_file: secrets.env", "secrets_file: \"\"", 1)
	c, err := Parse([]byte(text), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckServer(); err != nil {
		t.Error(err)
	}
	if c.Storage.Spool != filepath.Join("/data", "spool") || c.Storage.Index != filepath.Join("/data", "calls.json") {
		t.Errorf("data paths: %s, %s", c.Storage.Spool, c.Storage.Index)
	}
}

// TestReadmeShowsExample keeps the complete file in the README in step with the example.
func TestReadmeShowsExample(t *testing.T) {
	ex, err := os.ReadFile("../../examples/dialout.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	norm := func(b []byte) string { return strings.ReplaceAll(string(b), "\r\n", "\n") }
	if !strings.Contains(norm(readme), "```yaml\n"+norm(ex)+"```") {
		t.Error("README.md's complete file differs from examples/dialout.example.yaml: paste the example into it again")
	}
}

func TestMinimalExampleConfig(t *testing.T) {
	b, err := os.ReadFile("../../examples/dialout.minimal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"TWILIO_ACCOUNT_SID", "TWILIO_API_KEY_SID", "TWILIO_API_KEY_SECRET", "TWILIO_AUTH_TOKEN"} {
		t.Setenv(k, "x")
	}
	if _, err := Parse(b, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	// The commented-out S3 storage is valid too.
	text := strings.ReplaceAll(string(b), "# storage:", "storage:")
	text = strings.ReplaceAll(text, "#   s3_", "  s3_")
	c, err := Parse([]byte(text), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Storage.S3Bucket == "" || c.Storage.S3Region == "" || c.Storage.S3Prefix != "dialout/" {
		t.Errorf("storage = %+v", c.Storage)
	}
}
