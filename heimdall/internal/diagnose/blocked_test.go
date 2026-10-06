package diagnose

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func status(age time.Duration, cs corev1.ContainerStatus) corev1.Pod {
	cs.Name = "app"
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1", CreationTimestamp: metav1.NewTime(t0.Add(-age))},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{cs}}}
}

func waitingFor(reason string) corev1.ContainerState {
	return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "detail for " + reason}}
}

func lastExit(reason string, code int32) corev1.ContainerState {
	return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: reason, ExitCode: code}}
}

// Blocked must stop waits for failures that will not recover, and never
// for ones that still might.
func TestBlocked(t *testing.T) {
	for _, tc := range []struct {
		name string
		pod  corev1.Pod
		want Code
	}{
		{"crash loop", status(time.Minute, corev1.ContainerStatus{RestartCount: 3, State: waitingFor("CrashLoopBackOff"), LastTerminationState: lastExit("Error", 1)}), ContainerCrash},
		{"crash loop seen between restarts", status(time.Minute, corev1.ContainerStatus{RestartCount: 3, State: lastExit("Error", 1), LastTerminationState: lastExit("Error", 1)}), ContainerCrash},
		{"one-off job pod that exited", status(time.Minute, corev1.ContainerStatus{RestartCount: 0, State: lastExit("Error", 1)}), ""},
		{"out of memory seen between restarts", status(time.Minute, corev1.ContainerStatus{RestartCount: 2, State: lastExit("OOMKilled", 137)}), OutOfMemory},
		{"crash loop, first restarts", status(time.Minute, corev1.ContainerStatus{RestartCount: 2, State: waitingFor("CrashLoopBackOff"), LastTerminationState: lastExit("Error", 1)}), ""},
		{"out of memory twice", status(time.Minute, corev1.ContainerStatus{RestartCount: 2, State: waitingFor("CrashLoopBackOff"), LastTerminationState: lastExit("OOMKilled", 137)}), OutOfMemory},
		{"out of memory once", status(time.Minute, corev1.ContainerStatus{RestartCount: 1, LastTerminationState: lastExit("OOMKilled", 137)}), ""},
		{"recovered after out of memory", status(time.Minute, corev1.ContainerStatus{RestartCount: 2, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, LastTerminationState: lastExit("OOMKilled", 137)}), ""},
		{"warming up after out of memory", status(time.Minute, corev1.ContainerStatus{RestartCount: 2, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, LastTerminationState: lastExit("OOMKilled", 137)}), ""},
		{"pull failing for long", status(PullGrace, corev1.ContainerStatus{State: waitingFor("ImagePullBackOff")}), ImagePullFailed},
		{"pull failing briefly", status(30*time.Second, corev1.ContainerStatus{State: waitingFor("ErrImagePull")}), ""},
		{"image absent with never pull policy", status(PullGrace, corev1.ContainerStatus{State: waitingFor("ErrImageNeverPull")}), ImagePullFailed},
		{"invalid image name", status(time.Second, corev1.ContainerStatus{State: waitingFor("InvalidImageName")}), ImagePullFailed},
		{"missing secret for a while", status(ConfigErrorGrace, corev1.ContainerStatus{State: waitingFor("CreateContainerConfigError")}), ConfigInvalid},
		{"missing secret briefly", status(10*time.Second, corev1.ContainerStatus{State: waitingFor("CreateContainerConfigError")}), ""},
		{"starting", status(time.Second, corev1.ContainerStatus{State: waitingFor("ContainerCreating")}), ""},
	} {
		b := Blocked([]corev1.Pod{tc.pod}, t0)
		switch {
		case tc.want == "" && b != nil:
			t.Errorf("%s: blocked: %v", tc.name, b)
		case tc.want != "" && (b == nil || b.Code != tc.want):
			t.Errorf("%s: got %v, want %s", tc.name, b, tc.want)
		case b != nil && !strings.Contains(b.Error(), "container app of pod api-1"):
			t.Errorf("%s: message %q", tc.name, b.Error())
		}
	}
	terminating := status(time.Hour, corev1.ContainerStatus{RestartCount: 9, State: waitingFor("CrashLoopBackOff")})
	terminating.DeletionTimestamp = &metav1.Time{Time: t0}
	if b := Blocked([]corev1.Pod{terminating}, t0); b != nil {
		t.Errorf("a terminating pod (an old revision) blocked: %v", b)
	}
}
