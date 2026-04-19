package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
)

// newFlagSet creates a flag.FlagSet that suppresses default output (errors are handled by callers).
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// CommandBuilder provides common FlagSet creation, parsing, and validation
// patterns used across all CLI commands. It eliminates the repeated
// newFlagSet → Parse → NArg check → required check boilerplate.
type CommandBuilder struct {
	name   string // e.g. "maestro plan submit"
	usage  string // e.g. "usage: maestro plan submit --command-id <id> [...]"
	fs     *flag.FlagSet
	checks []validationCheck
}

type validationCheck struct {
	test    func() bool
	message string
}

// NewCommand creates a CommandBuilder for the given command.
// name is the full command name (e.g. "maestro plan submit").
// usage is the bare usage text (prefixed with "usage: " automatically).
func NewCommand(name, usage string) *CommandBuilder {
	return &CommandBuilder{
		name:  name,
		usage: "usage: " + usage,
		fs:    newFlagSet(name),
	}
}

// StringVar defines a string flag.
func (b *CommandBuilder) StringVar(p *string, name, value, usage string) {
	b.fs.StringVar(p, name, value, usage)
}

// IntVar defines an int flag.
func (b *CommandBuilder) IntVar(p *int, name string, value int, usage string) {
	b.fs.IntVar(p, name, value, usage)
}

// BoolVar defines a bool flag.
func (b *CommandBuilder) BoolVar(p *bool, name string, value bool, usage string) {
	b.fs.BoolVar(p, name, value, usage)
}

// Var defines a flag with a custom flag.Value.
func (b *CommandBuilder) Var(v flag.Value, name, usage string) {
	b.fs.Var(v, name, usage)
}

// RequiredString defines a string flag and registers a required check
// (the flag is considered missing if its value is empty after parsing).
func (b *CommandBuilder) RequiredString(p *string, name, usage string) {
	b.fs.StringVar(p, name, "", usage)
	b.checks = append(b.checks, validationCheck{
		test:    func() bool { return *p != "" },
		message: "--" + name + " is required",
	})
}

// RequiredInt defines an int flag with a sentinel default and registers
// a required check (the flag is considered missing if its value equals
// the sentinel after parsing).
func (b *CommandBuilder) RequiredInt(p *int, name string, sentinel int, usage string) {
	b.fs.IntVar(p, name, sentinel, usage)
	b.checks = append(b.checks, validationCheck{
		test:    func() bool { return *p != sentinel },
		message: "--" + name + " is required",
	})
}

// AddCheck registers a custom validation that runs after parsing.
// The message is used as-is in the error output.
func (b *CommandBuilder) AddCheck(message string, test func() bool) {
	b.checks = append(b.checks, validationCheck{
		test:    test,
		message: message,
	})
}

// Parse parses args, rejects unexpected positional args, and runs all
// registered validation checks. Returns a CLIError on failure.
// When -h or --help is passed, flag descriptions are included in the output.
func (b *CommandBuilder) Parse(args []string) error {
	if err := b.fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return b.helpMessage()
		}
		return b.withUsage(fmt.Sprintf("%v", err))
	}
	if b.fs.NArg() > 0 {
		return b.withUsage(fmt.Sprintf("unexpected argument: %s", b.fs.Arg(0)))
	}
	return b.validate()
}

// ParsePositional parses flags but does NOT reject positional args.
// Use NArg/Arg to inspect remaining positional args after calling this.
func (b *CommandBuilder) ParsePositional(args []string) error {
	if err := b.fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return b.helpMessage()
		}
		return b.withUsage(fmt.Sprintf("%v", err))
	}
	return nil
}

// Validate runs all registered validation checks.
func (b *CommandBuilder) Validate() error {
	return b.validate()
}

// NArg returns the number of remaining positional arguments after parsing.
func (b *CommandBuilder) NArg() int { return b.fs.NArg() }

// Arg returns the i-th remaining positional argument.
func (b *CommandBuilder) Arg(i int) string { return b.fs.Arg(i) }

// Errorf returns a CLIError with the command name prefix (no usage appended).
func (b *CommandBuilder) Errorf(format string, args ...any) error {
	return &CLIError{Code: 1, Msg: fmt.Sprintf(b.name+": "+format, args...)}
}

// UsageErrorf returns a CLIError with the command name and usage string.
func (b *CommandBuilder) UsageErrorf(format string, args ...any) error {
	return b.withUsage(fmt.Sprintf(format, args...))
}

func (b *CommandBuilder) validate() error {
	for _, c := range b.checks {
		if !c.test() {
			return b.withUsage(c.message)
		}
	}
	return nil
}

func (b *CommandBuilder) withUsage(msg string) error {
	return &CLIError{Code: 1, Msg: fmt.Sprintf("%s: %s\n%s", b.name, msg, b.usage)}
}

// helpMessage builds a help output with the usage string and flag descriptions.
func (b *CommandBuilder) helpMessage() error {
	var buf bytes.Buffer
	b.fs.SetOutput(&buf)
	b.fs.PrintDefaults()
	b.fs.SetOutput(io.Discard)

	msg := b.usage
	if buf.Len() > 0 {
		msg += "\n\nFlags:\n" + buf.String()
	}
	return &CLIError{Code: 1, Msg: msg}
}
