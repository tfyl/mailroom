package main

import (
	"os"
	"strings"
	"testing"
)

// pipeStdin puts a value on standard input the way a shell pipeline does, which is also what
// makes the read a non-terminal one.
func pipeStdin(t *testing.T, in string) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = w.WriteString(in)
		_ = w.Close()
	}()

	saved := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = saved; _ = r.Close() })
}

// An unattended deployment takes the password from a secret store through the environment
// and never opens standard input at all.
func TestPasswordFromTheEnvironment(t *testing.T) {
	t.Setenv("MAILROOM_LINK_PASSWORD", "hunter2")
	pipeStdin(t, "from-stdin")

	got, err := readPassword()
	if err != nil {
		t.Fatal(err)
	}
	if got != "hunter2" {
		t.Fatalf("readPassword() = %q; the environment should win", got)
	}
}

func TestPasswordPipedIn(t *testing.T) {
	t.Setenv("MAILROOM_LINK_PASSWORD", "")
	// An app password arrives with the spaces Google displays it with, and a newline from
	// the pipe.
	pipeStdin(t, "abcd efgh ijkl mnop\n")

	got, err := readPassword()
	if err != nil {
		t.Fatal(err)
	}
	if got != "abcdefghijklmnop" {
		t.Fatalf("readPassword() = %q", got)
	}
}

func TestNoPasswordAnywhereIsAnError(t *testing.T) {
	t.Setenv("MAILROOM_LINK_PASSWORD", "")
	pipeStdin(t, "\n")

	if _, err := readPassword(); err == nil {
		t.Fatal("an empty standard input should not have produced a password")
	} else if !strings.Contains(err.Error(), "MAILROOM_LINK_PASSWORD") {
		t.Fatalf("the error should say where a password can come from, got: %v", err)
	}
}
