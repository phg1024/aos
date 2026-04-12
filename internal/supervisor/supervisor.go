package supervisor

import (
	"context"
	"strconv"

	"aos/internal/kernel"
	"aos/internal/scheduler"
)

type ChildTask struct {
	Name string
	Spec kernel.ProcessSpec
}

type ChildResult struct {
	Name      string            `json:"name"`
	PID       string            `json:"pid"`
	Attempts  int               `json:"attempts"`
	Exit      kernel.ExitStatus `json:"exit"`
	Succeeded bool              `json:"succeeded"`
}

type Summary struct {
	Results []ChildResult `json:"results"`
}

type Node struct {
	Process  kernel.Process `json:"process"`
	Children []Node         `json:"children,omitempty"`
}

type Supervisor struct {
	Kernel    *kernel.Kernel
	Scheduler *scheduler.Scheduler
}

func (s *Supervisor) RunChildren(ctx context.Context, parentPID string, tasks []ChildTask, retryLimit int) (Summary, error) {
	type tracked struct {
		task     ChildTask
		attempts int
	}

	var order []string
	trackedByPID := make(map[string]*tracked)
	for _, task := range tasks {
		childSpec := task.Spec
		childSpec.ParentPID = parentPID
		child, err := s.Kernel.Spawn(childSpec)
		if err != nil {
			return Summary{}, err
		}
		order = append(order, child.PID)
		trackedByPID[child.PID] = &tracked{task: task, attempts: 1}
		s.Scheduler.Enqueue(child.PID)
	}

	for {
		if err := s.Scheduler.Run(ctx); err != nil {
			return Summary{}, err
		}

		var retrySpawned bool
		for _, pid := range append([]string(nil), order...) {
			info, ok := trackedByPID[pid]
			if !ok || info == nil {
				continue
			}
			process, err := s.Kernel.Get(pid)
			if err != nil {
				return Summary{}, err
			}
			if process.State != kernel.StateExited || process.ExitStatus == nil {
				continue
			}
			if process.ExitStatus.Retryable && process.ExitStatus.Code != 0 && info.attempts <= retryLimit {
				childSpec := info.task.Spec
				childSpec.ParentPID = parentPID
				childSpec.Metadata = cloneMap(childSpec.Metadata)
				if childSpec.Metadata == nil {
					childSpec.Metadata = map[string]string{}
				}
				childSpec.Metadata["retry_of"] = pid
				retryChild, err := s.Kernel.Spawn(childSpec)
				if err != nil {
					return Summary{}, err
				}
				order = append(order, retryChild.PID)
				trackedByPID[retryChild.PID] = &tracked{
					task:     info.task,
					attempts: info.attempts + 1,
				}
				delete(trackedByPID, pid)
				s.Scheduler.Enqueue(retryChild.PID)
				_ = s.Kernel.LogEvent(retryChild.PID, "supervisor.retry", map[string]string{
					"prior_pid": pid,
					"attempt":   strconv.Itoa(info.attempts + 1),
				})
				retrySpawned = true
			}
		}
		if !retrySpawned {
			break
		}
	}

	var summary Summary
	for _, pid := range order {
		info, ok := trackedByPID[pid]
		if !ok {
			continue
		}
		process, err := s.Kernel.Get(pid)
		if err != nil {
			return Summary{}, err
		}
		if process.ExitStatus == nil {
			continue
		}
		summary.Results = append(summary.Results, ChildResult{
			Name:      info.task.Name,
			PID:       pid,
			Attempts:  info.attempts,
			Exit:      *process.ExitStatus,
			Succeeded: process.ExitStatus.Code == 0,
		})
	}
	return summary, nil
}

func (s *Supervisor) Tree(rootPID string) (Node, error) {
	process, err := s.Kernel.Get(rootPID)
	if err != nil {
		return Node{}, err
	}
	children, err := s.Kernel.Children(rootPID)
	if err != nil {
		return Node{}, err
	}
	node := Node{Process: process}
	for _, child := range children {
		childNode, err := s.Tree(child.PID)
		if err != nil {
			return Node{}, err
		}
		node.Children = append(node.Children, childNode)
	}
	return node, nil
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
