// Package awscreds reads AWS credentials from a file that something else keeps fresh, such as a
// host timer that assumes a role and writes the temporary credentials into the container.
//
// The SDK reads a shared credentials file once and keeps those keys, and its credential_process
// runs the command through sh, which the image doesn't have. This provider re-reads the file
// instead, shortly before the keys it last read expire.
package awscreds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// Env names the file. When it's set, every AWS client dialout builds uses it.
const Env = "DIALOUT_AWS_CREDENTIALS_FILE"

// ExpiryWindow is how long before the expiration the file is read again.
const ExpiryWindow = 5 * time.Minute

// File reads credential_process JSON from Path:
//
//	{"Version": 1, "AccessKeyId": "…", "SecretAccessKey": "…", "SessionToken": "…", "Expiration": "2026-10-24T12:00:00Z"}
type File struct {
	Path string
}

// FromEnv returns a cached provider for the file in $DIALOUT_AWS_CREDENTIALS_FILE, or nil when
// it's unset (the SDK's default chain applies).
func FromEnv() aws.CredentialsProvider {
	p := os.Getenv(Env)
	if p == "" {
		return nil
	}
	return aws.NewCredentialsCache(&File{Path: p}, func(o *aws.CredentialsCacheOptions) {
		o.ExpiryWindow = ExpiryWindow
	})
}

func (f *File) Retrieve(context.Context) (aws.Credentials, error) {
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return aws.Credentials{}, fmt.Errorf("AWS credentials: %w", err)
	}
	c, err := parse(b, time.Now())
	if err != nil {
		return aws.Credentials{}, fmt.Errorf("AWS credentials %s: %w", f.Path, err)
	}
	c.Source = "dialout file " + f.Path
	return c, nil
}

func parse(b []byte, now time.Time) (aws.Credentials, error) {
	var v struct {
		Version         int
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string
		SessionToken    string
		Expiration      *time.Time
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return aws.Credentials{}, err
	}
	switch {
	case v.Version != 1:
		return aws.Credentials{}, errors.New("Version must be 1")
	case v.AccessKeyID == "" || v.SecretAccessKey == "":
		return aws.Credentials{}, errors.New("AccessKeyId or SecretAccessKey is empty")
	case v.Expiration != nil && !v.Expiration.After(now):
		return aws.Credentials{}, fmt.Errorf("expired at %s", v.Expiration.Format(time.RFC3339))
	}
	c := aws.Credentials{AccessKeyID: v.AccessKeyID, SecretAccessKey: v.SecretAccessKey, SessionToken: v.SessionToken}
	if v.Expiration != nil {
		c.CanExpire, c.Expires = true, *v.Expiration
	}
	return c, nil
}
