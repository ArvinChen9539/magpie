package tui

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/appdir"
)

func TestTUILogStaysOffTerminal(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		name := "file available"
		if blocked {
			name = "file unavailable"
		}
		t.Run(name, func(t *testing.T) {
			temp := t.TempDir()
			for _, env := range []string{"HOME", "USERPROFILE", "LOCALAPPDATA", "XDG_STATE_HOME"} {
				t.Setenv(env, temp)
			}
			path := filepath.Join(appdir.Logs(), "tui.log")
			if blocked {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			terminalPath := filepath.Join(temp, "terminal")
			terminal, err := os.Create(terminalPath)
			if err != nil {
				t.Fatal(err)
			}
			defer terminal.Close()
			previous := log.Writer()
			log.SetOutput(terminal)
			defer log.SetOutput(previous)

			lines := []string{
				"remote-magpie: its list changed there, fetched again: 71 models",
				"remote-magpie: fetched its list on schedule, 71 models",
				"gateway: drain finished",
			}
			programErr := errors.New("program stopped")
			if err := runSession(model{}, func(ctx context.Context) func() {
				log.Print(lines[0])
				return func() {
					if ctx.Err() != context.Canceled {
						t.Error("gateway waited before cancellation")
					}
					log.Print(lines[2])
				}
			}, func() error {
				log.Print(lines[1])
				return programErr
			}); err != programErr {
				t.Errorf("session error = %v, want %v", err, programErr)
			}
			const after = "logged after the TUI exited"
			log.Print(after)
			output, err := os.ReadFile(terminalPath)
			if err != nil {
				t.Fatal(err)
			}
			got := string(output)
			for _, line := range lines {
				if strings.Contains(got, line) {
					t.Errorf("background log reached the terminal: %s", line)
				}
			}
			if !strings.Contains(got, after) {
				t.Error("the previous log writer was not restored")
			}
			if blocked {
				return
			}
			output, err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range lines {
				if !strings.Contains(string(output), line) {
					t.Errorf("TUI log is missing %q", line)
				}
			}
			if strings.Contains(string(output), after) {
				t.Error("the TUI log kept receiving output after the session")
			}
		})
	}
}
