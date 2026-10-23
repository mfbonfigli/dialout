// Package config reads dialout.yaml: every setting but the secrets, which the file references as
// ${VAR} and which come from the environment or from a KEY=VALUE secrets file.
//
// Decoding is strict: an unknown key or a wrong type is an error, so a typo can't be silently
// ignored. Load checks everything a call needs, the prompt file included, so a mistake stops the
// command at start-up instead of failing a call.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mfbonfigli/dialout/internal/policy"
	"github.com/mfbonfigli/dialout/prompts"
)

// Env is the variable that names the configuration file when -config doesn't.
const Env = "DIALOUT_CONFIG"

// DataEnv names the folder for the spool, the stored calls and the history when the file doesn't
// set them; ~/.dialout when unset. The container sets it to /data.
const DataEnv = "DIALOUT_DATA"

// Config is dialout.yaml.
type Config struct {
	Owner       string  `yaml:"owner"`        // who the agent calls for, as said on the phone
	Timezone    string  `yaml:"timezone"`     // for the date in the prompt; the machine's when empty
	SecretsFile string  `yaml:"secrets_file"` // KEY=VALUE file loaded into the environment first
	Twilio      Twilio  `yaml:"twilio"`
	Engine      Engine  `yaml:"engine"`
	Prompt      Prompt  `yaml:"prompt"`
	Tunnel      Tunnel  `yaml:"tunnel"`
	Storage     Storage `yaml:"storage"`
	Server      Server  `yaml:"server"`
	Policy      Policy  `yaml:"policy"`

	Path     string         `yaml:"-"` // the file it was read from
	Location *time.Location `yaml:"-"` // Timezone, loaded
	Prompts  *prompts.Set   `yaml:"-"` // Prompt.File, loaded
}

type Twilio struct {
	AccountSID   string `yaml:"account_sid"`
	APIKeySID    string `yaml:"api_key_sid"`
	APIKeySecret string `yaml:"api_key_secret"`
	AuthToken    string `yaml:"auth_token"` // only to check X-Twilio-Signature
	From         string `yaml:"from"`       // caller ID: verified, or a number on the account
}

type Engine struct {
	Name       string     `yaml:"name"`     // sonic, elevenlabs or openai
	Language   string     `yaml:"language"` // the callee's language, for engines that transcribe separately
	Sonic      Sonic      `yaml:"sonic"`
	ElevenLabs ElevenLabs `yaml:"elevenlabs"`
	OpenAI     OpenAI     `yaml:"openai"`
}

type Sonic struct {
	Region string `yaml:"region"`
	Voice  string `yaml:"voice"`
}

type ElevenLabs struct {
	APIKey      string `yaml:"api_key"`
	AgentID     string `yaml:"agent_id"`
	VoiceID     string `yaml:"voice_id"`     // instead of the agent's voice
	VoiceGender string `yaml:"voice_gender"` // the prompt's grammatical gender: female or male
	LLM         string `yaml:"llm"`          // instead of the agent's model
	APIBase     string `yaml:"api_base"`
}

type OpenAI struct {
	APIKey      string `yaml:"api_key"`
	Model       string `yaml:"model"`
	Voice       string `yaml:"voice"`
	VoiceGender string `yaml:"voice_gender"` // only for a voice outside the built-in list
	APIBase     string `yaml:"api_base"`
}

type Prompt struct {
	File string `yaml:"file"` // empty: the built-in prompt
}

type Tunnel struct {
	Hostname    string `yaml:"hostname"`    // empty: a quick tunnel per call
	Token       string `yaml:"token"`       // the named tunnel's token, with a hostname
	Cloudflared string `yaml:"cloudflared"` // path; found in PATH when empty
	Listen      string `yaml:"listen"`      // the stream endpoint the tunnel forwards to
}

type Storage struct {
	Enabled        bool   `yaml:"enabled"`      // false: store nothing on disk, neither transcripts nor audio
	RecordAudio    bool   `yaml:"record_audio"` // with Enabled, the transcript and meta.json are always kept
	SegmentSeconds int    `yaml:"segment_seconds"`
	Spool          string `yaml:"spool"`
	Dir            string `yaml:"dir"`       // where finished calls go, unless S3Bucket
	S3Bucket       string `yaml:"s3_bucket"` // upload under calls/ in this bucket instead
	Index          string `yaml:"index"`     // the server's call history
}

type Server struct {
	MCPListen string `yaml:"mcp_listen"` // MCP over HTTP
	MCPToken  string `yaml:"mcp_token"`  // the bearer token every HTTP request must carry
}

type Policy struct {
	Source         string         `yaml:"source"` // file (the keys below); ssm is reserved
	Enabled        bool           `yaml:"enabled"`
	Mode           string         `yaml:"mode"`            // allowlist, or open: any dialable number
	Countries      []string       `yaml:"countries"`       // ISO codes of the countries numbers may be in
	DefaultCountry string         `yaml:"default_country"` // for numbers written without +
	NumberTypes    []string       `yaml:"number_types"`    // fixed, mobile, toll_free
	Blocklist      []string       `yaml:"blocklist"`       // numbers or prefixes never called
	Hours          string         `yaml:"hours"`           // open mode, unlisted numbers: e.g. 09:00-21:00
	Allowlist      []policy.Entry `yaml:"allowlist"`
	Limits         Limits         `yaml:"limits"`

	dialable  policy.Dialable // the keys above, checked
	blocklist []string
	hours     policy.Hours
}

type Limits struct {
	MaxMinutes     int `yaml:"max_minutes"`
	RingSeconds    int `yaml:"ring_seconds"`
	PerHour        int `yaml:"per_hour"`           // 0: no limit
	PerDay         int `yaml:"per_day"`            // 0: no limit
	PerNumberDay   int `yaml:"per_number_per_day"` // open mode, unlisted numbers; 0: no limit
	CalleeChars    int `yaml:"callee_chars"`
	OpeningChars   int `yaml:"opening_chars"`
	ObjectiveChars int `yaml:"objective_chars"`
	LimitsChars    int `yaml:"limits_chars"`
	ContextChars   int `yaml:"context_chars"`
}

// MaxMinutes is the longest call: below Sonic's 8-minute session, the shortest of the engines.
const MaxMinutes = 7

// defaults are the values of the keys a file leaves out.
func defaults() Config {
	return Config{
		Engine: Engine{Name: "sonic", Language: "it", Sonic: Sonic{Region: "eu-north-1", Voice: "beatrice"},
			ElevenLabs: ElevenLabs{VoiceGender: "female"}},
		Tunnel:  Tunnel{Listen: "127.0.0.1:8090"},
		Storage: Storage{Enabled: true, SegmentSeconds: 60},
		Server:  Server{MCPListen: ":8080"},
		Policy: Policy{Source: "file", Mode: string(policy.ModeAllowlist), Countries: []string{"IT"}, DefaultCountry: "IT",
			NumberTypes: []string{"fixed", "mobile"}, Hours: "09:00-21:00", Limits: Limits{
				MaxMinutes: 5, RingSeconds: 25, PerHour: 6, PerDay: 20, PerNumberDay: 2,
				CalleeChars: 100, OpeningChars: 300, ObjectiveChars: 500, LimitsChars: 500, ContextChars: 2000,
			}},
	}
}

// Find returns the configuration file to read: path when given, else $DIALOUT_CONFIG, else
// dialout/dialout.yaml in the user's configuration folder, else /etc/dialout/dialout.yaml.
func Find(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	if p := os.Getenv(Env); p != "" {
		return p, nil
	}
	var tried []string
	if dir, err := os.UserConfigDir(); err == nil {
		tried = append(tried, filepath.Join(dir, "dialout", "dialout.yaml"))
	}
	tried = append(tried, "/etc/dialout/dialout.yaml")
	for _, p := range tried {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no configuration file: pass -config, set %s, or create %s", Env, strings.Join(tried, " or "))
}

// Load reads, expands and checks a configuration file. Relative paths in it are relative to its
// folder.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c, err := Parse(b, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	c.Path = path
	return c, nil
}

// Parse decodes and checks a configuration; dir is the folder relative paths start from.
func Parse(data []byte, dir string) (*Config, error) {
	c := defaults()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the file is empty")
		}
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one YAML document")
	}

	rel := func(p *string) {
		if *p != "" && !filepath.IsAbs(*p) && !strings.HasPrefix(*p, "/") && !strings.HasPrefix(*p, `\`) {
			*p = filepath.Join(dir, *p)
		}
	}
	rel(&c.SecretsFile)
	if c.SecretsFile != "" {
		if err := LoadEnvFile(c.SecretsFile); err != nil {
			return nil, fmt.Errorf("secrets_file: %w", err)
		}
	}
	unset := expand(reflect.ValueOf(&c).Elem(), "")

	base := os.Getenv(DataEnv)
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".dialout")
	}
	for _, d := range []struct {
		p   *string
		def string
	}{
		{&c.Storage.Spool, filepath.Join(base, "spool")},
		{&c.Storage.Dir, filepath.Join(base, "calls")},
		{&c.Storage.Index, filepath.Join(base, "calls.json")},
	} {
		if *d.p == "" {
			*d.p = d.def
		} else {
			rel(d.p)
		}
	}
	rel(&c.Prompt.File)
	rel(&c.Tunnel.Cloudflared)

	if err := c.check(unset); err != nil {
		return nil, err
	}
	var err error
	if c.Location, err = loadLocation(c.Timezone); err != nil {
		return nil, err
	}
	if c.Prompts, err = prompts.Load(c.Prompt.File); err != nil {
		return nil, err
	}
	return &c, nil
}

func loadLocation(name string) (*time.Location, error) {
	if name == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("timezone: %w", err)
	}
	return loc, nil
}

// check validates the settings every command needs. unset maps a key to the variables it
// referenced that weren't set, for the error messages.
func (c *Config) check(unset map[string][]string) error {
	var errs []error
	need := func(key, v string) {
		if strings.TrimSpace(v) == "" {
			msg := key + " is empty"
			if vars := unset[key]; len(vars) > 0 {
				msg += " (" + strings.Join(vars, ", ") + " not set)"
			}
			errs = append(errs, errors.New(msg))
		}
	}
	oneOf := func(key, v string, allowed ...string) {
		if !slices.Contains(allowed, v) {
			errs = append(errs, fmt.Errorf("%s: %q isn't one of %s", key, v, strings.Join(allowed, ", ")))
		}
	}

	if err := prompts.ValidOwner(c.Owner); err != nil {
		errs = append(errs, fmt.Errorf("owner: %w", err))
	}
	need("twilio.account_sid", c.Twilio.AccountSID)
	need("twilio.api_key_sid", c.Twilio.APIKeySID)
	need("twilio.api_key_secret", c.Twilio.APIKeySecret)
	need("twilio.auth_token", c.Twilio.AuthToken)
	need("twilio.from", c.Twilio.From)

	oneOf("engine.name", c.Engine.Name, "sonic", "elevenlabs", "openai")
	need("engine.language", c.Engine.Language)
	switch c.Engine.Name {
	case "sonic":
		need("engine.sonic.region", c.Engine.Sonic.Region)
	case "elevenlabs":
		need("engine.elevenlabs.api_key", c.Engine.ElevenLabs.APIKey)
		need("engine.elevenlabs.agent_id", c.Engine.ElevenLabs.AgentID)
		oneOf("engine.elevenlabs.voice_gender", c.Engine.ElevenLabs.VoiceGender, "female", "male")
	case "openai":
		need("engine.openai.api_key", c.Engine.OpenAI.APIKey)
		oneOf("engine.openai.voice_gender", c.Engine.OpenAI.VoiceGender, "", "female", "male")
	}

	if c.Tunnel.Hostname != "" {
		need("tunnel.token", c.Tunnel.Token)
	}
	need("tunnel.listen", c.Tunnel.Listen)

	if !c.Storage.Enabled && c.Storage.RecordAudio {
		errs = append(errs, errors.New("storage.record_audio needs storage.enabled"))
	}
	if c.Storage.SegmentSeconds < 1 {
		errs = append(errs, errors.New("storage.segment_seconds must be at least 1"))
	}

	need("server.mcp_listen", c.Server.MCPListen)

	if c.Policy.Source != "file" {
		errs = append(errs, fmt.Errorf("policy.source: %q isn't supported; use file", c.Policy.Source))
	}
	oneOf("policy.mode", c.Policy.Mode, string(policy.ModeAllowlist), string(policy.ModeOpen))
	if err := c.Policy.compile(); err != nil {
		errs = append(errs, err)
	} else if al, err := policy.ValidateAllowlist(c.Policy.Allowlist, c.Policy.dialable); err != nil {
		errs = append(errs, fmt.Errorf("policy.allowlist: %w", err))
	} else {
		c.Policy.Allowlist = al
	}
	l := c.Policy.Limits
	switch {
	case l.MaxMinutes < 1 || l.MaxMinutes > MaxMinutes:
		errs = append(errs, fmt.Errorf("policy.limits.max_minutes must be between 1 and %d", MaxMinutes))
	case l.RingSeconds < 5 || l.RingSeconds > 120:
		errs = append(errs, errors.New("policy.limits.ring_seconds must be between 5 and 120"))
	case l.PerHour < 0 || l.PerDay < 0 || l.PerNumberDay < 0:
		errs = append(errs, errors.New("policy.limits.per_hour, per_day and per_number_per_day can't be negative"))
	case l.CalleeChars < 1 || l.OpeningChars < 1 || l.ObjectiveChars < 1 || l.LimitsChars < 1 || l.ContextChars < 1:
		errs = append(errs, errors.New("policy.limits: every size limit must be at least 1"))
	}
	return errors.Join(errs...)
}

// Check validates the configuration again, after a command changed it (e.g. -engine).
func (c *Config) Check() error { return c.check(nil) }

// CheckServer validates what the MCP server over HTTP needs on top: a token long enough.
func (c *Config) CheckServer() error {
	if len(c.Server.MCPToken) < 32 {
		return errors.New("server.mcp_token must be at least 32 characters (e.g. openssl rand -hex 32)")
	}
	return nil
}

// PolicyConfig is the policy in force.
func (c *Config) PolicyConfig() policy.Config {
	l := c.Policy.Limits
	return policy.Config{
		Enabled: c.Policy.Enabled, Mode: policy.Mode(c.Policy.Mode), Dialable: c.Policy.dialable,
		Allowlist: c.Policy.Allowlist, Blocklist: c.Policy.blocklist, Hours: c.Policy.hours, Location: c.Location,
		MaxPerNumberPerDay: l.PerNumberDay, MaxPerHour: l.PerHour, MaxPerDay: l.PerDay,
		MaxCallee: l.CalleeChars, MaxOpening: l.OpeningChars,
		MaxObjective: l.ObjectiveChars, MaxContext: l.ContextChars, MaxLimits: l.LimitsChars,
		MaxCallSeconds: l.MaxMinutes * 60,
	}
}

// compile checks the countries, number types, blocklist and hours, and keeps them in the form
// the policy uses.
func (p *Policy) compile() error {
	var errs []error
	if len(p.Countries) == 0 {
		errs = append(errs, errors.New("policy.countries is empty"))
	}
	var countries []string
	for _, c := range p.Countries {
		c = strings.ToUpper(strings.TrimSpace(c))
		if !policy.ValidCountry(c) {
			errs = append(errs, fmt.Errorf("policy.countries: %q isn't an ISO country code", c))
		}
		countries = append(countries, c)
	}
	def := strings.ToUpper(strings.TrimSpace(p.DefaultCountry))
	if def != "" && !policy.ValidCountry(def) {
		errs = append(errs, fmt.Errorf("policy.default_country: %q isn't an ISO country code", def))
	}
	if len(p.NumberTypes) == 0 {
		errs = append(errs, errors.New("policy.number_types is empty"))
	}
	var kinds []policy.Kind
	for _, t := range p.NumberTypes {
		k := policy.Kind(strings.TrimSpace(t))
		if !slices.Contains(policy.AllowableKinds, k) {
			errs = append(errs, fmt.Errorf("policy.number_types: %q isn't one of fixed, mobile, toll_free", t))
		}
		kinds = append(kinds, k)
	}
	p.dialable = policy.Dialable{Countries: countries, DefaultCountry: def, Kinds: kinds}
	p.blocklist = nil
	for _, b := range p.Blocklist {
		n, err := policy.NormalizeBlock(b)
		if err != nil {
			errs = append(errs, fmt.Errorf("policy.blocklist: %w", err))
		}
		p.blocklist = append(p.blocklist, n)
	}
	var err error
	if p.hours, err = policy.ParseHours(p.Hours); err != nil {
		errs = append(errs, fmt.Errorf("policy.%w", err))
	}
	return errors.Join(errs...)
}

// expand replaces ${VAR} with the variable's value in every string of v, after decoding, so a
// secret containing # or ": " can't break the YAML. $$ is a literal $. It returns, by key, the
// variables referenced but not set: they expand to nothing.
func expand(v reflect.Value, key string) map[string][]string {
	unset := map[string][]string{}
	var walk func(v reflect.Value, key string)
	walk = func(v reflect.Value, key string) {
		switch v.Kind() {
		case reflect.String:
			s, missing := expandString(v.String())
			v.SetString(s)
			if len(missing) > 0 {
				unset[key] = missing
			}
		case reflect.Struct:
			t := v.Type()
			for i := range t.NumField() {
				f := t.Field(i)
				name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
				if name == "-" || !f.IsExported() {
					continue
				}
				if name == "" {
					name = strings.ToLower(f.Name)
				}
				k := name
				if key != "" {
					k = key + "." + name
				}
				walk(v.Field(i), k)
			}
		case reflect.Slice:
			for i := range v.Len() {
				walk(v.Index(i), fmt.Sprintf("%s[%d]", key, i))
			}
		}
	}
	walk(v, key)
	return unset
}

func expandString(s string) (string, []string) {
	if !strings.Contains(s, "$") {
		return s, nil
	}
	var b strings.Builder
	var missing []string
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] != '$' || i+1 == len(s):
			b.WriteByte(s[i])
		case s[i+1] == '$':
			b.WriteByte('$')
			i++
		case s[i+1] == '{':
			end := strings.IndexByte(s[i+2:], '}')
			name := ""
			if end >= 0 {
				name = s[i+2 : i+2+end]
			}
			if !validName(name) {
				b.WriteByte(s[i])
				continue
			}
			v, ok := os.LookupEnv(name)
			if !ok || v == "" {
				missing = append(missing, name)
			}
			b.WriteString(v)
			i += 2 + end
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), missing
}

func validName(n string) bool {
	if n == "" {
		return false
	}
	for i, r := range n {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
