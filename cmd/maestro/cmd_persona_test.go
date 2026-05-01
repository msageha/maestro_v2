package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunPersona_MissingSubcommand(t *testing.T) {
	err := runPersona([]string{})
	if err == nil {
		t.Fatal("expected error when subcommand is missing")
	}
	if !strings.Contains(err.Error(), "usage: maestro persona <list>") {
		t.Errorf("expected persona usage error, got: %v", err)
	}
}

func TestRunPersona_UnknownSubcommand(t *testing.T) {
	err := runPersona([]string{"unknown"})
	if err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
	if !strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("expected unknown subcommand error, got: %v", err)
	}
}

func TestRunPersonaList(t *testing.T) {
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	personaDir := filepath.Join(maestroDir, "persona")
	if err := os.MkdirAll(personaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(personaDir, "researcher.md"),
		[]byte("---\nname: Researcher Display Name\ndescription: Research persona\n---\nBody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(personaDir, "implementer.md"),
		[]byte("---\nname: implementer\ndescription: Implement persona\n---\nBody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(personaDir, "notes.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runPersonaList([]string{})

	w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "researcher\tResearch persona") {
		t.Errorf("output should contain researcher persona, got: %s", output)
	}
	if strings.Contains(output, "Researcher Display Name") {
		t.Errorf("output should use persona IDs, not display names, got: %s", output)
	}
	if !strings.Contains(output, "implementer\tImplement persona") {
		t.Errorf("output should contain implementer persona, got: %s", output)
	}
	if strings.Contains(output, "notes") {
		t.Errorf("output should ignore non-markdown files, got: %s", output)
	}
}

func TestRunPersonaList_NoPersonas(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".maestro"), 0o755); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runPersonaList([]string{})

	w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "No personas found.") {
		t.Errorf("expected no personas message, got: %s", output)
	}
}
