package awscreds

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/mfbonfigli/dialout/internal/config"
)

func TestParse(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, json, err string
	}{
		{"temporary", `{"Version":1,"AccessKeyId":"ASIA","SecretAccessKey":"s","SessionToken":"t","Expiration":"2026-10-24T12:30:00Z"}`, ""},
		{"aws cli offset", `{"Version":1,"AccessKeyId":"ASIA","SecretAccessKey":"s","SessionToken":"t","Expiration":"2026-10-24T12:30:00+00:00"}`, ""},
		{"static", `{"Version":1,"AccessKeyId":"AKIA","SecretAccessKey":"s"}`, ""},
		{"expired", `{"Version":1,"AccessKeyId":"ASIA","SecretAccessKey":"s","Expiration":"2026-10-24T11:59:00Z"}`, "expired"},
		{"version", `{"Version":2,"AccessKeyId":"ASIA","SecretAccessKey":"s"}`, "Version"},
		{"no key", `{"Version":1,"SecretAccessKey":"s"}`, "empty"},
		{"not json", `[default]`, "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parse([]byte(tc.json), now)
			if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("got %v, want %q", err, tc.err)
			}
			if err == nil && c.CanExpire != strings.Contains(tc.json, "Expiration") {
				t.Fatalf("CanExpire = %v", c.CanExpire)
			}
		})
	}
}

// The cache keeps the keys it read until ExpiryWindow before they expire, then reads the file
// again: what the host rewrote in between is picked up without a restart.
func TestRefresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	write := func(key string, exp time.Time) {
		t.Helper()
		b := fmt.Sprintf(`{"Version":1,"AccessKeyId":%q,"SecretAccessKey":"s","SessionToken":"t","Expiration":%q}`, key, exp.UTC().Format(time.RFC3339))
		if err := os.WriteFile(path, []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cache := aws.NewCredentialsCache(&File{Path: path}, func(o *aws.CredentialsCacheOptions) {
		o.ExpiryWindow = ExpiryWindow
	})
	get := func() string {
		t.Helper()
		c, err := cache.Retrieve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return c.AccessKeyID
	}

	// KEY1 enters the expiry window two seconds from now.
	write("KEY1", time.Now().Add(ExpiryWindow+2*time.Second))
	if k := get(); k != "KEY1" {
		t.Fatalf("first read: %s", k)
	}
	write("KEY2", time.Now().Add(time.Hour))
	if k := get(); k != "KEY1" {
		t.Fatalf("still valid: want the cached KEY1, got %s", k)
	}
	time.Sleep(3 * time.Second)
	if k := get(); k != "KEY2" {
		t.Fatalf("in the expiry window: want KEY2 from the file, got %s", k)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(Env, "")
	if FromEnv() != nil {
		t.Fatal("unset: want nil, the SDK's default chain")
	}
	t.Setenv(Env, "/config/aws/credentials.json")
	if FromEnv() == nil {
		t.Fatal("set: want a provider")
	}
}

func TestExpiryWindowOutlastsACall(t *testing.T) {
	if ExpiryWindow <= time.Duration(config.MaxMinutes)*time.Minute {
		t.Fatalf("ExpiryWindow %s, calls last up to %d minutes", ExpiryWindow, config.MaxMinutes)
	}
}
