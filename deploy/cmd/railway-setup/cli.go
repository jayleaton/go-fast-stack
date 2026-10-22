package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// minCLI is the oldest Railway CLI this tool is verified against (Go IaC
// evaluation with a context, variables.ignore, --environment on config).
var minCLI = [3]int{5, 71, 0}

// cli runs the Railway CLI. Every call carries RAILWAY_PROJECT_ID and an
// explicit --environment, so nothing depends on `railway link` state.
// Secret values only ever travel on stdin, never in argv or output.
type cli struct {
	bin       string
	projectID string
	extraEnv  []string
	stdout    io.Writer // where streamed commands write; stderr with --json
}

func newCLI() *cli {
	bin := os.Getenv("RAILWAY_BIN")
	if bin == "" {
		bin = "railway"
	}
	return &cli{bin: bin, stdout: os.Stdout}
}

func (c *cli) cmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Env = append(os.Environ(), c.extraEnv...)
	if c.projectID != "" {
		cmd.Env = append(cmd.Env, "RAILWAY_PROJECT_ID="+c.projectID)
	}
	return cmd
}

// output runs a command and returns its stdout. stdin may be nil.
func (c *cli) output(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := c.cmd(ctx, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// stderr only: stdout of variable commands holds values.
		return stdout.Bytes(), fmt.Errorf("railway %s: %w%s", args[0], err, tail(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// stream runs a command with the terminal attached (plan/apply output).
// It returns the exit code; err is only for failures to start.
func (c *cli) stream(ctx context.Context, args ...string) (int, error) {
	return c.streamIn(ctx, "", args...)
}

// streamIn is stream with a working directory.
func (c *cli) streamIn(ctx context.Context, dir string, args ...string) (int, error) {
	cmd := c.cmd(ctx, args...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, c.stdout, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

func (c *cli) json(ctx context.Context, out any, args ...string) error {
	b, err := c.output(ctx, nil, append(args, "--json")...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("railway %s: decoding JSON: %w", args[0], err)
	}
	return nil
}

// gql runs a GraphQL document through `railway api`; variables go on stdin.
// HTTP 200 with errors is a failure.
func (c *cli) gql(ctx context.Context, query string, vars map[string]any, out any) error {
	if vars == nil {
		vars = map[string]any{}
	}
	in, err := json.Marshal(vars)
	if err != nil {
		return err
	}
	b, runErr := c.output(ctx, in, "api", query, "--variables", "@-")
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		if runErr != nil {
			return runErr
		}
		return fmt.Errorf("railway api: decoding response: %w", err)
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, len(resp.Errors))
		for i, e := range resp.Errors {
			msgs[i] = e.Message
		}
		return fmt.Errorf("railway api: %s", strings.Join(msgs, "; "))
	}
	if runErr != nil {
		return runErr
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(resp.Data, out)
}

func (c *cli) checkVersion(ctx context.Context) error {
	b, err := c.output(ctx, nil, "--version")
	if err != nil {
		return fmt.Errorf("the Railway CLI is required (https://docs.railway.com/cli): %w", err)
	}
	fields := strings.Fields(string(b)) // "railway 5.71.0"
	if len(fields) == 0 {
		return errors.New("railway --version printed nothing")
	}
	v := strings.Split(strings.TrimPrefix(fields[len(fields)-1], "v"), ".")
	var got [3]int
	for i := 0; i < 3 && i < len(v); i++ {
		got[i], _ = strconv.Atoi(v[i])
	}
	for i := range got {
		if got[i] != minCLI[i] {
			if got[i] < minCLI[i] {
				return fmt.Errorf("railway CLI %s is too old: need %d.%d.%d or newer (railway upgrade)", strings.TrimSpace(string(b)), minCLI[0], minCLI[1], minCLI[2])
			}
			break
		}
	}
	// whoami can fail transiently while Railway's API is degraded.
	for i := range 3 {
		if _, err = c.output(ctx, nil, "whoami"); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i+1) * 2 * time.Second):
		}
	}
	return fmt.Errorf("not logged in to Railway: run railway login: %w", err)
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 2000 {
		s = s[len(s)-2000:]
	}
	if s == "" {
		return ""
	}
	return "\n" + s
}
