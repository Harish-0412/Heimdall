package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"

	"github.com/heimdall-dev/heimdall/internal/diagnose"
	"github.com/heimdall-dev/heimdall/internal/engine"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

func secretValues(ctx context.Context, c typedcore.CoreV1Interface, ns string) ([]string, error) {
	values, err := diagnose.SecretValues(ctx, c, ns)
	if err != nil {
		return nil, fmt.Errorf("cannot load secret values for log redaction")
	}
	return values, nil
}

func writeLogs(ctx context.Context, cfg *rest.Config, e *engine.Engine, spec engine.Spec, workload, container string, tail int64, follow bool, out io.Writer) error {
	if tail < 1 || tail > 10000 {
		return fmt.Errorf("--tail must be between 1 and 10000")
	}
	pods, err := e.LogPods(ctx, spec, workload)
	if err != nil {
		return err
	}
	if len(pods) == 0 {
		return fmt.Errorf("no matching pods")
	}
	if follow && (len(pods) != 1 || container == "") {
		return fmt.Errorf("--follow requires exactly one matching pod and an explicit --container")
	}
	rc := rest.CopyConfig(cfg)
	if follow {
		rc.Timeout = 0
	}
	c, err := typedcore.NewForConfig(rc)
	if err != nil {
		return err
	}
	ns := pods[0].GetNamespace()
	// Read pod specs before opening any stream: all referenced secret values
	// must be available for the same redaction policy used by diagnostics.
	var live []corev1.Pod
	var specs []corev1.PodSpec
	for _, p := range pods {
		pod, err := c.Pods(ns).Get(ctx, p.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}
		live = append(live, *pod)
		specs = append(specs, pod.Spec)
	}
	values, err := diagnose.SecretValues(ctx, c, ns, specs...)
	if err != nil {
		return fmt.Errorf("cannot load secret values for log redaction")
	}
	for _, pod := range live {
		for _, ct := range pod.Spec.Containers {
			if container != "" && ct.Name != container {
				continue
			}
			limit := int64(1 << 20)
			opts := &corev1.PodLogOptions{Container: ct.Name, TailLines: &tail, Follow: follow}
			if !follow {
				opts.LimitBytes = &limit
			}
			stream, err := c.Pods(ns).GetLogs(pod.Name, opts).Stream(ctx)
			if err != nil {
				return fmt.Errorf("cannot open logs for %s/%s", pod.Name, ct.Name)
			}
			scanner := bufio.NewScanner(stream)
			scanner.Buffer(make([]byte, 4096), 1<<20)
			for scanner.Scan() {
				if _, err = fmt.Fprintf(out, "[%s/%s] %s\n", pod.Name, ct.Name, engine.Redact(scanner.Text(), values)); err != nil {
					break
				}
			}
			scanErr := scanner.Err()
			_ = stream.Close()
			if err != nil {
				return err
			}
			if scanErr != nil {
				return fmt.Errorf("log stream failed or line exceeded 1 MiB")
			}
		}
	}
	return nil
}
