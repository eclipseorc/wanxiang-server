package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadReadsValuesAndPreservesExistingEnvironment(t *testing.T) {
	const existingKey = "WANXIANG_ENVFILE_EXISTING_TEST"
	t.Setenv(existingKey, "process-value")
	file := filepath.Join(t.TempDir(), ".env")
	contents := "# comment\nWANXIANG_ENVFILE_PLAIN_TEST=hello world\nWANXIANG_ENVFILE_QUOTED_TEST=\"value with spaces\"\n" + existingKey + "=file-value\n"
	if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}

	if err := Load(file); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"WANXIANG_ENVFILE_PLAIN_TEST":  "hello world",
		"WANXIANG_ENVFILE_QUOTED_TEST": "value with spaces",
		existingKey:                     "process-value",
	} {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestLoadMissingFileIsOptional(t *testing.T) {
	if err := Load(filepath.Join(t.TempDir(), "missing.env")); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsMalformedLine(t *testing.T) {
	file := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(file, []byte("not a variable\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Load(file); err == nil {
		t.Fatal("Load() expected malformed-line error")
	}
}
