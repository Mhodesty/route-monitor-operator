package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// AgentManager manages the lifecycle of the RHOBS Synthetics Agent
type AgentManager struct {
	cmd       *exec.Cmd
	agentPath string
	apiURLs   []string
	stopChan  chan struct{}
	started   bool
	ctx       context.Context
	cancel    context.CancelFunc
}

// NewAgentManager creates a new manager for the agent
func NewAgentManager(apiURLs []string) *AgentManager {
	ctx, cancel := context.WithCancel(context.Background())

	// Get agent path with priority:
	// 1. RHOBS_SYNTHETICS_AGENT_PATH environment variable (for local development)
	// 2. Go module cache (automatic) - will be copied to temp dir
	// 3. Relative path fallback
	agentPath := os.Getenv("RHOBS_SYNTHETICS_AGENT_PATH")
	var needsCopy bool

	if agentPath == "" {
		// Try to find the agent in the Go module cache
		if modulePath, err := getModulePath("github.com/rhobs/rhobs-synthetics-agent"); err == nil && modulePath != "" {
			agentPath = modulePath
			needsCopy = true // Module cache is read-only, need to copy
		} else {
			// Fall back to relative path
			agentPath = "../../rhobs-synthetics-agent"
		}
	}

	// If using module cache, copy to a writable temp directory
	if needsCopy {
		tempDir := filepath.Join("/tmp", "rhobs-synthetics-agent-build")
		if err := copyDir(agentPath, tempDir); err == nil {
			agentPath = tempDir
		}
		// If copy fails, will try to use module cache directly (may fail at build)
	}

	return &AgentManager{
		agentPath: agentPath,
		apiURLs:   apiURLs,
		stopChan:  make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
	}
}

// Start builds and starts the agent
func (m *AgentManager) Start() error {
	if m.started {
		return fmt.Errorf("agent already started")
	}

	// Build the agent first
	if err := m.buildAgent(); err != nil {
		return fmt.Errorf("failed to build agent: %w", err)
	}

	// Start the agent
	if err := m.startAgent(); err != nil {
		return fmt.Errorf("failed to start agent: %w", err)
	}

	// Give agent time to initialize
	time.Sleep(2 * time.Second)

	m.started = true
	return nil
}

// Stop shuts down the agent
func (m *AgentManager) Stop() error {
	if !m.started {
		return nil
	}

	m.cancel()
	close(m.stopChan)

	if m.cmd != nil && m.cmd.Process != nil {
		// Try graceful shutdown first
		if err := m.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			// Force kill if graceful shutdown fails
			_ = m.cmd.Process.Kill()
		}
		_ = m.cmd.Wait()
	}

	m.started = false
	return nil
}

// buildAgent builds the agent binary
func (m *AgentManager) buildAgent() error {
	// Run make clean first to remove any old binaries
	cleanCmd := exec.CommandContext(m.ctx, "make", "clean")
	cleanCmd.Dir = m.agentPath
	_ = cleanCmd.Run() // Ignore errors from clean

	// Now build
	cmd := exec.CommandContext(m.ctx, "make", "build")
	cmd.Dir = m.agentPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to build agent: %w", err)
	}

	return nil
}

// startAgent starts the agent process
func (m *AgentManager) startAgent() error {
	binaryPath := filepath.Join(m.agentPath, "rhobs-synthetics-agent")

	args := []string{
		"start",
		"--log-level", "debug",
		"--interval", "2s",
		"--graceful-timeout", "5s",
		"--api-urls", strings.Join(m.apiURLs, ","),
	}

	m.cmd = exec.CommandContext(m.ctx, binaryPath, args...)

	// Capture output for debugging
	stdout, err := m.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := m.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := m.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start agent process: %w", err)
	}

	// Start goroutines to handle output
	go m.handleOutput("stdout", stdout)
	go m.handleOutput("stderr", stderr)

	return nil
}

// handleOutput handles stdout/stderr from the agent process
func (m *AgentManager) handleOutput(prefix string, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		select {
		case <-m.stopChan:
			return
		default:
			// Only print agent output if we want to debug
			// fmt.Printf("[Agent %s] %s\n", prefix, scanner.Text())
		}
	}
}
