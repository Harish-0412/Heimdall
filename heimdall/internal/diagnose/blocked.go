package diagnose

import (
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Thresholds after which a pod failure is treated as permanent.
const (
	CrashRestarts    = 3               // restarts in a crash loop
	OOMRestarts      = 2               // out-of-memory kills
	PullGrace        = 2 * time.Minute // pulling may recover (an image pushed late, a registry blip)
	ConfigErrorGrace = time.Minute     // a secret may be created moments later
)

// Blocker is a pod failure that will not resolve without new input.
type Blocker struct {
	Code      Code
	Pod       string
	Container string
	Reason    string // Kubernetes' reason: CrashLoopBackOff, OOMKilled, ...
	Message   string
}

func (b *Blocker) Error() string {
	s := fmt.Sprintf("%s: container %s of pod %s: %s", b.Code, b.Container, b.Pod, b.Reason)
	if b.Message != "" {
		s += ": " + firstLine(b.Message)
	}
	return s
}

// Blocked returns the first permanent failure among pods, or nil. The engine
// uses it to stop waiting for a workload that cannot become ready (instead of
// waiting out the step timeout); transient states, such as one restart or a
// pull still retrying within PullGrace, do not count.
func Blocked(pods []corev1.Pod, now time.Time) *Blocker {
	pods = slices.Clone(pods)
	slices.SortFunc(pods, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	for _, p := range pods {
		if p.DeletionTimestamp != nil {
			continue
		}
		age := now.Sub(p.CreationTimestamp.Time)
		for _, cs := range statuses(&p) {
			if cs.Ready || cs.State.Running != nil {
				continue // a recovered instance must not inherit a historical OOM
			}
			b := &Blocker{Pod: p.Name, Container: cs.Name}
			w, last := cs.State.Waiting, cs.LastTerminationState.Terminated
			// Between restarts the kubelet reports either the back-off
			// (Waiting: CrashLoopBackOff) or the exit itself (Terminated):
			// both mean the container keeps dying.
			if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
				last = t
			}
			crashing := (w != nil && w.Reason == "CrashLoopBackOff") || (cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0)
			switch {
			case crashing && last != nil && last.Reason == "OOMKilled" && cs.RestartCount >= OOMRestarts:
				b.Code, b.Reason = OutOfMemory, "OOMKilled"
				b.Message = fmt.Sprintf("killed for exceeding its memory limit, %d restarts", cs.RestartCount)
			case crashing && cs.RestartCount >= CrashRestarts:
				b.Code, b.Reason = ContainerCrash, "CrashLoopBackOff"
				if last != nil {
					b.Message = fmt.Sprintf("exit code %d, %d restarts", last.ExitCode, cs.RestartCount)
				}
			case w != nil && w.Reason == "InvalidImageName":
				b.Code, b.Reason, b.Message = ImagePullFailed, w.Reason, w.Message
			case w != nil && (w.Reason == "ImagePullBackOff" || w.Reason == "ErrImagePull" || w.Reason == "ErrImageNeverPull") && age >= PullGrace:
				b.Code, b.Reason, b.Message = ImagePullFailed, w.Reason, w.Message
			case w != nil && w.Reason == "CreateContainerConfigError" && age >= ConfigErrorGrace:
				b.Code, b.Reason, b.Message = ConfigInvalid, w.Reason, w.Message
			default:
				continue
			}
			return b
		}
	}
	return nil
}
