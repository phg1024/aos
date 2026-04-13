package tokenizer

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sync"

	"aos/internal/runtime"
)

type Counter interface {
	CountMessages(messages []runtime.Message) (CountResult, error)
	Close() error
}

type CountResult struct {
	Tokens int  `json:"tokens"`
	Exact  bool `json:"exact"`
}

type Sidecar struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
}

type request struct {
	Messages []runtime.Message `json:"messages"`
}

type response struct {
	Tokens int    `json:"tokens"`
	Exact  bool   `json:"exact"`
	Error  string `json:"error"`
}

func Start(pythonBin, repoRoot, model string) (*Sidecar, error) {
	if pythonBin == "" {
		pythonBin = "python3"
	}
	scriptPath := filepath.Join(repoRoot, "scripts", "tokenizer_sidecar.py")
	cmd := exec.Command(pythonBin, scriptPath, "--model", model)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	sidecar := &Sidecar{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
	}
	line, err := sidecar.stdout.ReadString('\n')
	if err != nil {
		_ = cmd.Wait()
		return nil, err
	}
	var ready response
	if err := json.Unmarshal([]byte(line), &ready); err != nil {
		errLine, _ := io.ReadAll(stderr)
		_ = cmd.Wait()
		return nil, fmt.Errorf("tokenizer sidecar invalid ready message: %w (%s)", err, string(errLine))
	}
	if ready.Error != "" {
		errLine, _ := io.ReadAll(stderr)
		_ = cmd.Wait()
		return nil, errors.New(ready.Error + " " + string(errLine))
	}
	return sidecar, nil
}

func (s *Sidecar) CountMessages(messages []runtime.Message) (CountResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	req := request{Messages: messages}
	payload, err := json.Marshal(req)
	if err != nil {
		return CountResult{}, err
	}
	if _, err := s.stdin.Write(append(payload, '\n')); err != nil {
		return CountResult{}, err
	}
	line, err := s.stdout.ReadString('\n')
	if err != nil {
		return CountResult{}, err
	}
	var resp response
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return CountResult{}, err
	}
	if resp.Error != "" {
		return CountResult{}, errors.New(resp.Error)
	}
	return CountResult{Tokens: resp.Tokens, Exact: resp.Exact}, nil
}

func (s *Sidecar) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.cmd != nil {
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		if err := s.cmd.Wait(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return err
			}
		}
	}
	return nil
}
