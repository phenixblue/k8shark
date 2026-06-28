package diagnose

import "encoding/json"

// PodHealth is a compact summary of one pod's state, derived from a pod object
// body. Used by the diagnose rules and (via aliases) the web UI.
type PodHealth struct {
	Phase      string            // "Running", "Pending", "Succeeded", "Failed", "Unknown"
	Ready      int               // ready container count
	Total      int               // total container count
	Restarts   int               // sum of restart counts
	Issues     []string          // unique reasons: "CrashLoopBackOff", "OOMKilled", "ImagePullBackOff", …
	Containers []ContainerHealth // per-container detail
}

// ContainerHealth is the per-container slice of PodHealth.
type ContainerHealth struct {
	Name           string `json:"name"`
	Image          string `json:"image,omitempty"`
	Ready          bool   `json:"ready"`
	RestartCount   int    `json:"restart_count"`
	IsInit         bool   `json:"is_init,omitempty"`
	State          string `json:"state"` // "Running" | "Waiting" | "Terminated"
	StateReason    string `json:"state_reason,omitempty"`
	StateMessage   string `json:"state_message,omitempty"`
	LastTerminated string `json:"last_terminated,omitempty"` // reason of lastState.terminated if present
	LastExitCode   int    `json:"last_exit_code,omitempty"`
}

// IsHealthy reports whether a pod is in a benign state. A pod with zero issues
// is considered healthy.
func (h PodHealth) IsHealthy() bool { return len(h.Issues) == 0 }

// ClassifyPod parses a pod body and returns its PodHealth. Returns zero
// PodHealth on a malformed body — callers should treat that as "unknown".
func ClassifyPod(body []byte) PodHealth {
	var pod struct {
		Status struct {
			Phase                 string            `json:"phase"`
			ContainerStatuses     []containerStatus `json:"containerStatuses"`
			InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &pod); err != nil {
		return PodHealth{}
	}

	h := PodHealth{Phase: pod.Status.Phase}
	seenIssue := map[string]bool{}
	addIssue := func(reason string) {
		if reason == "" || seenIssue[reason] {
			return
		}
		seenIssue[reason] = true
		h.Issues = append(h.Issues, reason)
	}

	switch h.Phase {
	case "Failed":
		addIssue("Failed")
	case "Unknown":
		addIssue("Unknown")
	case "Pending":
		// Pending alone isn't an issue — that's just "still scheduling".
		// We surface a container-level reason below if there is one.
	}

	consume := func(cs containerStatus, isInit bool) {
		h.Total++
		h.Restarts += cs.RestartCount
		c := ContainerHealth{
			Name:         cs.Name,
			Image:        cs.Image,
			Ready:        cs.Ready,
			RestartCount: cs.RestartCount,
			IsInit:       isInit,
		}
		if cs.Ready {
			h.Ready++
		}
		switch {
		case cs.State.Running != nil:
			c.State = "Running"
		case cs.State.Waiting != nil:
			c.State = "Waiting"
			c.StateReason = cs.State.Waiting.Reason
			c.StateMessage = cs.State.Waiting.Message
			addIssue(cs.State.Waiting.Reason)
		case cs.State.Terminated != nil:
			c.State = "Terminated"
			c.StateReason = cs.State.Terminated.Reason
			c.StateMessage = cs.State.Terminated.Message
			c.LastExitCode = cs.State.Terminated.ExitCode
			// A terminated init container that exited 0 is normal — don't flag.
			if !isInit || cs.State.Terminated.ExitCode != 0 {
				addIssue(cs.State.Terminated.Reason)
			}
		}
		if cs.LastState.Terminated != nil {
			c.LastTerminated = cs.LastState.Terminated.Reason
			if c.LastExitCode == 0 {
				c.LastExitCode = cs.LastState.Terminated.ExitCode
			}
		}
		h.Containers = append(h.Containers, c)
	}

	for _, cs := range pod.Status.InitContainerStatuses {
		consume(cs, true)
	}
	for _, cs := range pod.Status.ContainerStatuses {
		consume(cs, false)
	}
	return h
}

type containerStatus struct {
	Name         string         `json:"name"`
	Image        string         `json:"image"`
	Ready        bool           `json:"ready"`
	RestartCount int            `json:"restartCount"`
	State        containerState `json:"state"`
	LastState    containerState `json:"lastState"`
}

type containerState struct {
	Running    *struct{}              `json:"running,omitempty"`
	Waiting    *containerStateWaiting `json:"waiting,omitempty"`
	Terminated *containerStateTerm    `json:"terminated,omitempty"`
}

type containerStateWaiting struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type containerStateTerm struct {
	Reason   string `json:"reason"`
	Message  string `json:"message"`
	ExitCode int    `json:"exitCode"`
}
