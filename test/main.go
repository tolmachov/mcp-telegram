package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/tolmachov/mcp-telegram/internal"
)

type command struct {
	name    string
	cmd     string
	waitFor bool
}

var testCases = map[string][]command{
	"search": {
		{
			name:    "Initialize",
			cmd:     `{"jsonrpc":"2.0","method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0.1.0"}},"id":0}`,
			waitFor: true,
		},
		{
			name:    "Initialized notification",
			cmd:     `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			waitFor: false,
		},
		{
			name:    "Search chats",
			cmd:     `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"SearchChats","arguments":{"query":"друзья","limit":10}},"id":1}`,
			waitFor: true,
		},
	},
	"summary": {
		{
			name:    "Initialize",
			cmd:     `{"jsonrpc":"2.0","method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0.1.0"}},"id":0}`,
			waitFor: true,
		},
		{
			name:    "Initialized notification",
			cmd:     `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			waitFor: false,
		},
		{
			name:    "List chats",
			cmd:     `{"jsonrpc":"2.0","method":"resources/read","params":{"uri":"telegram://chats"},"id":1}`,
			waitFor: true,
		},
		{
			name:    "Summarize chat",
			cmd:     `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"SummarizeChat","arguments":{"chat_id":-1001346855510,"goal":"общий контекст обсуждений","period":"month"}},"id":2}`,
			waitFor: true,
		},
	},
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run main.go <test-name>")
		fmt.Println("Available tests:")
		for name := range testCases {
			fmt.Printf("  - %s\n", name)
		}
		os.Exit(1)
	}

	testName := os.Args[1]
	commands, ok := testCases[testName]
	if !ok {
		fmt.Printf("Unknown test: %s\n", testName)
		fmt.Println("Available tests:")
		for name := range testCases {
			fmt.Printf("  - %s\n", name)
		}
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Fatalf("failed to load .env file: %v", err)
	}

	// Create pipes for communication
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()

	// Channel to signal response received
	responseCh := make(chan json.RawMessage, 1)

	// Start server in goroutine
	go func() {
		args := []string{"mcp-telegram", "run"}
		if err := internal.New(stdinReader, stdoutWriter, os.Stderr).Run(ctx, args); err != nil {
			log.Printf("server error: %v", err)
		}
	}()

	// Read responses in goroutine
	go func() {
		scanner := bufio.NewScanner(stdoutReader)
		// Increase buffer for large responses
		buf := make([]byte, 0, 1024*1024)
		scanner.Buffer(buf, 10*1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			// Pretty print JSON response
			var resp map[string]any
			if err := json.Unmarshal([]byte(line), &resp); err == nil {
				pretty, _ := json.MarshalIndent(resp, "", "  ")
				fmt.Printf("Response:\n%s\n\n", string(pretty))

				// Signal that response was received (only for responses with id)
				if _, hasID := resp["id"]; hasID {
					select {
					case responseCh <- json.RawMessage(line):
					default:
					}
				}
			} else {
				fmt.Printf("Response: %s\n\n", line)
			}
		}
		if err := scanner.Err(); err != nil {
			log.Printf("scanner error: %v", err)
		}
	}()

	// Send commands sequentially, waiting for responses
	for _, c := range commands {
		fmt.Printf("=== %s ===\n", c.name)
		fmt.Printf("Sending: %s\n\n", c.cmd)

		_, err := stdinWriter.Write([]byte(c.cmd + "\n"))
		if err != nil {
			log.Fatalf("failed to write command: %v", err)
		}

		if c.waitFor {
			// Wait for response with timeout
			select {
			case <-responseCh:
				fmt.Println("--- Response received, continuing... ---")
			case <-time.After(10 * time.Minute):
				log.Fatalf("timeout waiting for response to: %s", c.name)
			case <-ctx.Done():
				return
			}
		} else {
			// Small delay for notifications
			time.Sleep(100 * time.Millisecond)
		}
	}

	fmt.Println("=== All commands completed! Press Ctrl+C to exit ===")

	// Wait for context cancellation (Ctrl+C)
	cancel()
	_ = stdinWriter.Close()
}
