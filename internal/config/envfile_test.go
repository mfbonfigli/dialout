package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.env")
	os.WriteFile(path, []byte(`# comment

HC_PLAIN=abc
export HC_EXPORTED = spaced
HC_SINGLE='[{"number": "+39 333", "label": "x"}]'
HC_DOUBLE="a=b"
HC_SET=from-file
`), 0o600)
	t.Setenv("HC_SET", "from-env")
	for _, k := range []string{"HC_PLAIN", "HC_EXPORTED", "HC_SINGLE", "HC_DOUBLE"} {
		t.Setenv(k, "") // registers the clean-up
		os.Unsetenv(k)
	}
	if err := LoadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"HC_PLAIN":    "abc",
		"HC_EXPORTED": "spaced",
		"HC_SINGLE":   `[{"number": "+39 333", "label": "x"}]`,
		"HC_DOUBLE":   "a=b",
		"HC_SET":      "from-env", // the environment wins
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	os.WriteFile(path, []byte("no equals sign\n"), 0o600)
	if err := LoadEnvFile(path); err == nil {
		t.Error("malformed line accepted")
	}
}
