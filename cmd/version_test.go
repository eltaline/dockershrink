package cmd

import (
	"bytes"
	"testing"
)

func TestVersionCmd(t *testing.T) {
	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"version"})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if len(out) == 0 {
		t.Fatal("expected version output, got empty string")
	}
}

func TestVersionValues(t *testing.T) {
	if Version == "" {
		t.Fatal("Version should not be empty")
	}
	if Commit == "" {
		t.Fatal("Commit should not be empty")
	}
	if Date == "" {
		t.Fatal("Date should not be empty")
	}
}
