package render

import (
	corev1 "k8s.io/api/core/v1"
)

// smoke runs every smoke test as a Job inside the namespace, so service names
// resolve as hostnames. Tests run in parallel in one step, from a minimal
// toolbox image (a shell and curl), and see the platform variables (PR, SHA,
// public URLs) but no database credentials or application secrets.
func (b *builder) smoke() Stage {
	if len(b.cfg.SmokeTests) == 0 {
		return Stage{Name: StageSmoke}
	}
	var objs []Object
	for _, t := range b.cfg.SmokeTests {
		c := container("smoke", toolboxImage.ref(b.platform.ImageMirror), true)
		c.Command = []string{"sh", "-c", t.Command}
		c.Env = b.platformEnv()
		c.Resources = b.fixed("100m", "128Mi")
		c.VolumeMounts = []corev1.VolumeMount{mount("tmp", "/tmp")}
		spec := b.podSpec(toolboxImage.uid, toolboxImage.gid, corev1.RestartPolicyNever,
			[]corev1.Volume{scratch("tmp", tmpSize)}, c)
		objs = append(objs, b.job(generationName(b.ctx.Generation, "heimdall-smoke", t.Name),
			objectName("heimdall-smoke", t.Name), componentSmokeTest, StageSmoke, t.Timeout.Std(), 0, spec))
	}
	return Stage{Name: StageSmoke, Steps: []Step{{Name: StepRun, Objects: objs}}}
}
