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

func (cli *cli) cmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, cli.bin, args...)
	cmd.Env = append(os.Environ(), cli.extraEnv...)
	if cli.projectID != "" {
		cmd.Env = append(cmd.Env, "RAILWAY_PROJECT_ID="+cli.projectID)
	}
	return cmd
}

// output runs a command and returns its stdout. stdin may be nil.
func (cli *cli) output(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := cli.cmd(ctx, args...)
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
func (cli *cli) stream(ctx context.Context, args ...string) (int, error) {
	return cli.streamIn(ctx, "", args...)
}

// streamIn is stream with a working directory.
func (cli *cli) streamIn(ctx context.Context, dir string, args ...string) (int, error) {
	cmd := cli.cmd(ctx, args...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, cli.stdout, os.Stderr
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

func (cli *cli) json(ctx context.Context, out any, args ...string) error {
	output, err := cli.output(ctx, nil, append(args, "--json")...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(output, out); err != nil {
		return fmt.Errorf("railway %s: decoding JSON: %w", args[0], err)
	}
	return nil
}

// gql runs a GraphQL document through `railway api`; variables go on stdin.
// HTTP 200 with errors is a failure.
func (cli *cli) gql(ctx context.Context, query string, vars map[string]any, out any) error {
	if vars == nil {
		vars = map[string]any{}
	}
	in, err := json.Marshal(vars)
	if err != nil {
		return err
	}
	output, runErr := cli.output(ctx, in, "api", query, "--variables", "@-")
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(output, &resp); err != nil {
		if runErr != nil {
			return runErr
		}
		return fmt.Errorf("railway api: decoding response: %w", err)
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, len(resp.Errors))
		for index, item := range resp.Errors {
			msgs[index] = item.Message
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

func (cli *cli) checkVersion(ctx context.Context) error {
	output, err := cli.output(ctx, nil, "--version")
	if err != nil {
		return fmt.Errorf("the Railway CLI is required (https://docs.railway.com/cli): %w", err)
	}
	fields := strings.Fields(string(output)) // "railway 5.71.0"
	if len(fields) == 0 {
		return errors.New("railway --version printed nothing")
	}
	values := strings.Split(strings.TrimPrefix(fields[len(fields)-1], "v"), ".")
	var got [3]int
	for index := 0; index < 3 && index < len(values); index++ {
		got[index], _ = strconv.Atoi(values[index])
	}
	for index := range got {
		if got[index] != minCLI[index] {
			if got[index] < minCLI[index] {
				return fmt.Errorf("railway CLI %s is too old: need %d.%d.%d or newer (railway upgrade)", strings.TrimSpace(string(output)), minCLI[0], minCLI[1], minCLI[2])
			}
			break
		}
	}
	// whoami can fail transiently while Railway's API is degraded.
	for index := range 3 {
		if _, err = cli.output(ctx, nil, "whoami"); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(index+1) * 2 * time.Second):
		}
	}
	return fmt.Errorf("not logged in to Railway: run railway login: %w", err)
}

func tail(text string) string {
	const maxTail = 2000 // bytes of a failed command's stderr kept in its error
	text = strings.TrimSpace(text)
	if len(text) > maxTail {
		text = text[len(text)-maxTail:]
	}
	if text == "" {
		return ""
	}
	return "\n" + text
}
