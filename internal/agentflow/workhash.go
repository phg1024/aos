package agentflow

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"aos/internal/capability"
)

// WorkHashEvent is the kernel event the engine records on a verifier's
// process when it exits: a hash of the work as the verifier left it. The
// verdict is bound to it; a different hash when the root exits makes a
// pass stale, whoever changed the work.
const WorkHashEvent = "verification.work_hashed"

// The most the work may hold for its hash: past either, the work cannot be
// bound and a pass is unverified. Spec contract v2 already refuses a work
// scope of / under a required gate.
const (
	MaxWorkFiles       = 200_000
	MaxWorkBytes int64 = 8 << 30
)

var errWorkTooLarge = errors.New("the work is too large to bind")

// WorkHash is the work's content at one moment.
type WorkHash struct {
	SHA256 string `json:"sha256,omitempty"`
	Files  int    `json:"files"`
	Bytes  int64  `json:"bytes"`
	// Err says why the work could not be hashed; SHA256 is then empty.
	Err string `json:"error,omitempty"`
}

// workScopes is what the verdict binds: the write scopes of every agent
// that can change the work, the entry agent's included, as req derives
// them. Left out are the verifier's own scopes (its reports), the entry
// agent's write paths that name files rather than directories (its own log
// and summary, which are its account of the run, not the work), and the
// protected state directory.
func (e *Engine) workScopes(spec WorkflowSpec, req RunRequest) (scopes, excluded []string) {
	verifier := spec.Verification.Verifier
	for _, agent := range spec.Agents {
		own := effectiveWriteScopes(agent, req)
		switch {
		case agent.ID == verifier:
			excluded = append(excluded, own...)
			continue
		case agent.ID == spec.EntryAgent:
			named := map[string]bool{}
			for _, path := range agent.Capabilities.WritePaths {
				named[capability.ResolvePath(path)] = true
			}
			for _, scope := range own {
				if info, err := os.Stat(scope); named[scope] && (err != nil || !info.IsDir()) {
					excluded = append(excluded, scope)
				} else {
					scopes = append(scopes, scope)
				}
			}
			continue
		}
		scopes = append(scopes, own...)
	}
	if e.Tools != nil {
		for _, path := range e.Tools.Protected {
			excluded = append(excluded, capability.ResolvePath(path))
		}
	}
	slices.Sort(scopes)
	scopes = slices.Compact(scopes)
	// A scope inside another is hashed with it.
	var outer []string
	for _, scope := range scopes {
		if len(outer) == 0 || !pathsWithin(scope, outer[len(outer)-1]) {
			outer = append(outer, scope)
		}
	}
	slices.Sort(excluded)
	return outer, slices.Compact(excluded)
}

// hashWork hashes every entry under scopes, but excluded ones: each
// directory's path, each symlink's target, and each file's path, mode, size
// and contents. A scope that does not exist is hashed as missing.
func hashWork(scopes, excluded []string) WorkHash {
	var out WorkHash
	h := sha256.New()
	left := func(path string) bool {
		for _, ex := range excluded {
			if pathsWithin(path, ex) {
				return true
			}
		}
		return false
	}
	for _, root := range scopes {
		if _, err := os.Lstat(root); err != nil {
			fmt.Fprintf(h, "missing %s\n", root)
			continue
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				fmt.Fprintf(h, "error %s %v\n", path, err)
				return nil
			}
			if left(path) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				fmt.Fprintf(h, "error %s %v\n", path, err)
				return nil
			}
			switch {
			case entry.IsDir():
				fmt.Fprintf(h, "d %s %o\n", path, info.Mode().Perm())
			case info.Mode()&fs.ModeSymlink != 0:
				target, _ := os.Readlink(path)
				fmt.Fprintf(h, "l %s %s\n", path, target)
			case info.Mode().IsRegular():
				out.Files++
				out.Bytes += info.Size()
				if out.Files > MaxWorkFiles || out.Bytes > MaxWorkBytes {
					return errWorkTooLarge
				}
				sum, err := fileSHA256(path)
				if err != nil {
					fmt.Fprintf(h, "error %s %v\n", path, err)
					return nil
				}
				fmt.Fprintf(h, "f %s %o %d %s\n", path, info.Mode().Perm(), info.Size(), sum)
			default:
				fmt.Fprintf(h, "o %s %s\n", path, info.Mode().Type())
			}
			return nil
		})
		if errors.Is(err, errWorkTooLarge) {
			return WorkHash{Files: out.Files, Bytes: out.Bytes, Err: fmt.Sprintf("%v: more than %d files or %d bytes under %s", err, MaxWorkFiles, MaxWorkBytes, strings.Join(scopes, ", "))}
		}
	}
	out.SHA256 = hex.EncodeToString(h.Sum(nil))
	return out
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// recordWorkHash hashes the work as verifierPID left it, on its process.
func (e *Engine) recordWorkHash(spec WorkflowSpec, req RunRequest, verifierPID string) error {
	scopes, excluded := e.workScopes(spec, req)
	hash := hashWork(scopes, excluded)
	data := map[string]string{
		"sha256":   hash.SHA256,
		"files":    strconv.Itoa(hash.Files),
		"bytes":    strconv.FormatInt(hash.Bytes, 10),
		"scopes":   strings.Join(scopes, "\n"),
		"excluded": strings.Join(excluded, "\n"),
	}
	if hash.Err != "" {
		data["error"] = hash.Err
	}
	_, err := e.Kernel.RecordEvent(verifierPID, WorkHashEvent, data)
	return err
}

// recordedWorkHashes are the hashes recorded at verifier exits, by PID.
func (e *Engine) recordedWorkHashes() (map[string]WorkHash, error) {
	events, err := e.Kernel.Events()
	if err != nil {
		return nil, err
	}
	hashes := map[string]WorkHash{}
	for _, event := range events {
		if event.Type != WorkHashEvent {
			continue
		}
		files, _ := strconv.Atoi(event.Data["files"])
		bytes, _ := strconv.ParseInt(event.Data["bytes"], 10, 64)
		hashes[event.ProcessID] = WorkHash{SHA256: event.Data["sha256"], Files: files, Bytes: bytes, Err: event.Data["error"]}
	}
	return hashes, nil
}
