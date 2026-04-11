package main

import (
	"errors"
	"strings"
	"testing"
)

func TestCommandBuilder_ParseNoFlags(t *testing.T) {
	t.Parallel()
	cmd := NewCommand("maestro test", "maestro test")
	if err := cmd.Parse(nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCommandBuilder_ParseUnknownFlag(t *testing.T) {
	t.Parallel()
	cmd := NewCommand("maestro test", "maestro test")
	err := cmd.Parse([]string{"--unknown"})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "maestro test:") {
		t.Errorf("expected command name prefix, got: %s", ce.Msg)
	}
	if !strings.Contains(ce.Msg, "usage: maestro test") {
		t.Errorf("expected usage string, got: %s", ce.Msg)
	}
}

func TestCommandBuilder_ParseUnexpectedArg(t *testing.T) {
	t.Parallel()
	cmd := NewCommand("maestro test", "maestro test")
	err := cmd.Parse([]string{"extra"})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "unexpected argument: extra") {
		t.Errorf("expected 'unexpected argument' message, got: %s", ce.Msg)
	}
}

func TestCommandBuilder_RequiredString(t *testing.T) {
	t.Parallel()

	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		cmd := NewCommand("maestro test", "maestro test --name <val>")
		var name string
		cmd.RequiredString(&name, "name", "")
		err := cmd.Parse(nil)
		if err == nil {
			t.Fatal("expected error")
		}
		var ce *CLIError
		if !errors.As(err, &ce) {
			t.Fatalf("expected CLIError, got %T: %v", err, err)
		}
		if !strings.Contains(ce.Msg, "--name is required") {
			t.Errorf("expected '--name is required', got: %s", ce.Msg)
		}
	})

	t.Run("provided", func(t *testing.T) {
		t.Parallel()
		cmd := NewCommand("maestro test", "maestro test --name <val>")
		var name string
		cmd.RequiredString(&name, "name", "")
		if err := cmd.Parse([]string{"--name", "foo"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "foo" {
			t.Errorf("name = %q, want %q", name, "foo")
		}
	})
}

func TestCommandBuilder_RequiredInt(t *testing.T) {
	t.Parallel()

	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		cmd := NewCommand("maestro test", "maestro test --epoch <n>")
		var epoch int
		cmd.RequiredInt(&epoch, "epoch", -1, "")
		err := cmd.Parse(nil)
		if err == nil {
			t.Fatal("expected error")
		}
		var ce *CLIError
		if !errors.As(err, &ce) {
			t.Fatalf("expected CLIError, got %T: %v", err, err)
		}
		if !strings.Contains(ce.Msg, "--epoch is required") {
			t.Errorf("expected '--epoch is required', got: %s", ce.Msg)
		}
	})

	t.Run("zero valid", func(t *testing.T) {
		t.Parallel()
		cmd := NewCommand("maestro test", "maestro test --epoch <n>")
		var epoch int
		cmd.RequiredInt(&epoch, "epoch", -1, "")
		if err := cmd.Parse([]string{"--epoch", "0"}); err != nil {
			t.Fatalf("unexpected error: epoch 0 should be valid: %v", err)
		}
		if epoch != 0 {
			t.Errorf("epoch = %d, want 0", epoch)
		}
	})
}

func TestCommandBuilder_AddCheck(t *testing.T) {
	t.Parallel()
	cmd := NewCommand("maestro test", "maestro test --a <v> --b <v>")
	var a, b string
	cmd.StringVar(&a, "a", "", "")
	cmd.StringVar(&b, "b", "", "")
	cmd.AddCheck("all required flags must be set", func() bool {
		return a != "" && b != ""
	})

	err := cmd.Parse([]string{"--a", "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "all required flags must be set") {
		t.Errorf("expected 'all required flags must be set', got: %s", ce.Msg)
	}
}

func TestCommandBuilder_ParsePositional(t *testing.T) {
	t.Parallel()
	cmd := NewCommand("maestro test", "maestro test <arg>")
	if err := cmd.ParsePositional([]string{"foo", "bar"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd.NArg() != 2 {
		t.Errorf("NArg() = %d, want 2", cmd.NArg())
	}
	if cmd.Arg(0) != "foo" {
		t.Errorf("Arg(0) = %q, want %q", cmd.Arg(0), "foo")
	}
}

func TestCommandBuilder_Errorf(t *testing.T) {
	t.Parallel()
	cmd := NewCommand("maestro test", "maestro test")
	err := cmd.Errorf("invalid --id: %s", "bad")
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	want := "maestro test: invalid --id: bad"
	if ce.Msg != want {
		t.Errorf("Msg = %q, want %q", ce.Msg, want)
	}
	// Errorf should NOT include usage
	if strings.Contains(ce.Msg, "usage:") {
		t.Error("Errorf should not include usage string")
	}
}

func TestCommandBuilder_UsageErrorf(t *testing.T) {
	t.Parallel()
	cmd := NewCommand("maestro test", "maestro test <arg>")
	err := cmd.UsageErrorf("missing argument")
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "maestro test: missing argument") {
		t.Errorf("expected command name prefix, got: %s", ce.Msg)
	}
	if !strings.Contains(ce.Msg, "usage: maestro test <arg>") {
		t.Errorf("expected usage string, got: %s", ce.Msg)
	}
}

func TestCommandBuilder_MultipleFlags(t *testing.T) {
	t.Parallel()
	cmd := NewCommand("maestro test", "maestro test --name <n> [--verbose]")
	var name string
	var verbose bool
	cmd.RequiredString(&name, "name", "")
	cmd.BoolVar(&verbose, "verbose", false, "")

	if err := cmd.Parse([]string{"--name", "foo", "--verbose"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "foo" {
		t.Errorf("name = %q, want %q", name, "foo")
	}
	if !verbose {
		t.Error("verbose should be true")
	}
}

func TestCommandBuilder_CheckOrder(t *testing.T) {
	t.Parallel()
	// First registered check should fail first
	cmd := NewCommand("maestro test", "maestro test")
	var a, b string
	cmd.RequiredString(&a, "a", "")
	cmd.RequiredString(&b, "b", "")

	err := cmd.Parse(nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "--a is required") {
		t.Errorf("expected first check to fail, got: %s", ce.Msg)
	}
}

func TestCommandBuilder_Validate(t *testing.T) {
	t.Parallel()
	cmd := NewCommand("maestro test", "maestro test --x <v>")
	var x string
	cmd.RequiredString(&x, "x", "")

	// ParsePositional does not run validation
	if err := cmd.ParsePositional(nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Validate runs checks explicitly
	err := cmd.Validate()
	if err == nil {
		t.Fatal("expected error from Validate")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "--x is required") {
		t.Errorf("expected '--x is required', got: %s", ce.Msg)
	}
}
