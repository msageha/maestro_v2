package status

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/uds"
	maestroyaml "github.com/msageha/maestro_v2/internal/yaml"
)

type FormationStatus struct {
	Daemon  DaemonStatus  `json:"daemon"`
	Agents  []AgentStatus `json:"agents,omitempty"`
	Queues  []QueueStatus `json:"queues,omitempty"`
}

type DaemonStatus struct {
	Running bool   `json:"running"`
	Pid     string `json:"pid,omitempty"`
}

type AgentStatus struct {
	ID     string `json:"id"`
	Role   string `json:"role"`
	Model  string `json:"model"`
	Status string `json:"status"`
}

type QueueStatus struct {
	Name       string `json:"name"`
	Pending    int    `json:"pending"`
	InProgress int    `json:"in_progress"`
}

// Run checks the formation status and prints it.
func Run(maestroDir string, jsonOutput bool) error {
	status := FormationStatus{}

	// Check daemon status via UDS ping
	sockPath := filepath.Join(maestroDir, uds.DefaultSocketName)
	status.Daemon = checkDaemon(sockPath)

	// Get agent status from tmux
	status.Agents = getAgentStatuses()

	// Get queue depth
	status.Queues = getQueueDepths(maestroDir)

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}

	printStatus(status)
	return nil
}

func checkDaemon(sockPath string) DaemonStatus {
	client := uds.NewClient(sockPath)
	resp, err := client.SendCommand("ping", nil)
	if err != nil {
		return DaemonStatus{Running: false}
	}
	if resp.Success {
		return DaemonStatus{Running: true}
	}
	return DaemonStatus{Running: false}
}

func getAgentStatuses() []AgentStatus {
	// Check if maestro tmux session exists
	out, err := exec.Command("tmux", "list-panes", "-s", "-t", "maestro", "-F",
		"#{@agent_id}\t#{@role}\t#{@model}\t#{@status}").CombinedOutput()
	if err != nil {
		return nil
	}

	var agents []AgentStatus
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 4 {
			continue
		}
		// Skip empty entries (panes without user variables)
		if parts[0] == "" {
			continue
		}
		agents = append(agents, AgentStatus{
			ID:     parts[0],
			Role:   parts[1],
			Model:  parts[2],
			Status: parts[3],
		})
	}
	return agents
}

type queueFile struct {
	Commands      []queueEntry `yaml:"commands,omitempty"`
	Tasks         []queueEntry `yaml:"tasks,omitempty"`
	Notifications []queueEntry `yaml:"notifications,omitempty"`
}

type queueEntry struct {
	Status string `yaml:"status"`
}

func getQueueDepths(maestroDir string) []QueueStatus {
	queueDir := filepath.Join(maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return nil
	}

	var queues []QueueStatus
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		filePath := filepath.Join(queueDir, entry.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			log.Printf("status: failed to read %s: %v", entry.Name(), err)
			continue
		}

		// Validate schema header (accept any queue file type)
		if err := maestroyaml.ValidateSchemaHeaderFromBytes(data, ""); err != nil {
			log.Printf("status: invalid schema in %s: %v", entry.Name(), err)
			continue
		}

		var qf queueFile
		if err := yaml.Unmarshal(data, &qf); err != nil {
			log.Printf("status: failed to parse %s: %v", entry.Name(), err)
			continue
		}

		// Collect all entries from whichever field is populated
		var allEntries []queueEntry
		allEntries = append(allEntries, qf.Commands...)
		allEntries = append(allEntries, qf.Tasks...)
		allEntries = append(allEntries, qf.Notifications...)

		var pending, inProgress int
		for _, e := range allEntries {
			switch e.Status {
			case "pending":
				pending++
			case "in_progress":
				inProgress++
			}
		}

		name := strings.TrimSuffix(entry.Name(), ".yaml")
		queues = append(queues, QueueStatus{
			Name:       name,
			Pending:    pending,
			InProgress: inProgress,
		})
	}

	return queues
}

func printStatus(s FormationStatus) {
	// Daemon
	if s.Daemon.Running {
		fmt.Println("Daemon: running")
	} else {
		fmt.Println("Daemon: stopped")
	}

	// Agents
	if len(s.Agents) > 0 {
		fmt.Println("\nAgents:")
		for _, a := range s.Agents {
			fmt.Printf("  %-14s  role=%-12s  model=%-6s  status=%s\n",
				a.ID, a.Role, a.Model, a.Status)
		}
	} else {
		fmt.Println("\nAgents: none")
	}

	// Queues
	if len(s.Queues) > 0 {
		fmt.Println("\nQueues:")
		fmt.Printf("  %-14s  %7s  %11s\n", "NAME", "PENDING", "IN_PROGRESS")
		for _, q := range s.Queues {
			fmt.Printf("  %-14s  %7d  %11d\n", q.Name, q.Pending, q.InProgress)
		}
	}
}
