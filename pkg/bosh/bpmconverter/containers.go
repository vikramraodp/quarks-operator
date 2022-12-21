package bpmconverter

import (
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"code.cloudfoundry.org/quarks-utils/pkg/names"

	"code.cloudfoundry.org/quarks-operator/pkg/bosh/bpm"
	bdm "code.cloudfoundry.org/quarks-operator/pkg/bosh/manifest"
	"code.cloudfoundry.org/quarks-operator/pkg/kube/util/logrotate"
	"code.cloudfoundry.org/quarks-operator/pkg/kube/util/operatorimage"
)

func (c *ContainerFactoryImpl) JobsToContainers(
	jobs []bdm.Job,
	defaultVolumeMounts []corev1.VolumeMount,
	bpmDisks bdm.Disks,
) ([]corev1.Container, error) {
	if c.errand {
		return c.errandsToContainers(jobs, defaultVolumeMounts, bpmDisks)
	}
	return c.jobsToContainers(jobs, defaultVolumeMounts, bpmDisks)
}

// JobsToContainers creates a list of Containers for corev1.PodSpec Containers field.
func (c *ContainerFactoryImpl) jobsToContainers(
	jobs []bdm.Job,
	defaultVolumeMounts []corev1.VolumeMount,
	bpmDisks bdm.Disks,
) ([]corev1.Container, error) {
	var containers []corev1.Container

	// wait for that many drain stamps to appear
	n := 0
	for _, job := range jobs {
		bpmConfig, ok := c.bpmConfigs[job.Name]
		if !ok {
			return nil, errors.Errorf("failed to lookup bpm config for bosh job '%s' in bpm configs", job.Name)
		}
		if len(bpmConfig.Processes) > 0 {
			n++
		}
	}
	drainStampCount := strconv.Itoa(n)

	// each job can produce multiple BPM process containers
	for _, job := range jobs {
		jobImage, err := c.releaseImageProvider.GetReleaseImage(c.instanceGroupName, job.Name)
		if err != nil {
			return []corev1.Container{}, err
		}

		bpmConfig, ok := c.bpmConfigs[job.Name]
		if !ok {
			return nil, errors.Errorf("failed to lookup bpm config for bosh job '%s' in bpm configs", job.Name)
		}
		if len(bpmConfig.Processes) < 1 {
			// we won't create any containers for this job
			continue
		}

		jobDisks := bpmDisks.Filter("job_name", job.Name)
		ephemeralMount, persistentDiskMount := jobDisks.BPMMounts()

		for processIndex, process := range bpmConfig.Processes {
			// extra volume mounts for this container
			processDisks := jobDisks.Filter("process_name", process.Name)
			volumeMounts := deduplicateVolumeMounts(proccessVolumentMounts(defaultVolumeMounts, processDisks, ephemeralMount, persistentDiskMount))

			// The post-start script should be executed only once per job, so we set it up in the first
			// process container. (container-run wrapper)
			var postStart postStart
			if processIndex == 0 {
				conditionProperty := bpmConfig.PostStart.Condition
				if conditionProperty != nil && conditionProperty.Exec != nil && len(conditionProperty.Exec.Command) > 0 {
					postStart.condition = &postStartCmd{
						Name: conditionProperty.Exec.Command[0],
						Arg:  conditionProperty.Exec.Command[1:],
					}
				}

				postStart.command = &postStartCmd{
					Name: filepath.Join(VolumeJobsDirMountPath, job.Name, "bin", "post-start"),
				}
			}

			command, args := generateBPMCommand(job.Name, &process, postStart)
			// each bpm process gets one container
			container, err := bpmProcessContainer(
				job.Name,
				process.Name,
				jobImage,
				process,
				command,
				args,
				volumeMounts,
				bpmConfig.Run.HealthCheck,
				job.Properties.Quarks.Envs,
				bpmConfig.Run.SecurityContext.DeepCopy(),
			)
			if err != nil {
				return []corev1.Container{}, err
			}

			// Setup the job drain script handler, on the first bpm
			// container. There is only one BOSH drain script per
			// job.
			// Theoretically that container might be missing
			// proccessVolumentMounts for the job's drain script.
			if processIndex == 0 {
				container.Lifecycle.PreStop = newDrainScript(job.Name, drainStampCount)
			} else {
				// all the other containers also should not terminate
				container.Lifecycle.PreStop = newDrainWait(drainStampCount)
			}

			containers = append(containers, *container.DeepCopy())
		}
	}

	// When disableLogSidecar is true, it will stop
	// appending the sidecar, default behaviour is to
	// colocate it always in the pod.
	if !c.disableLogSidecar {
		logsTailer := logsTailerContainer()
		containers = append(containers, logsTailer)
	}

	return containers, nil
}

// errandsToContainers creates k8s.Containers for BOSH Errands
func (c *ContainerFactoryImpl) errandsToContainers(
	jobs []bdm.Job,
	defaultVolumeMounts []corev1.VolumeMount,
	bpmDisks bdm.Disks,
) ([]corev1.Container, error) {
	var containers []corev1.Container

	// each job can produce multiple BPM process containers
	for _, job := range jobs {
		jobImage, err := c.releaseImageProvider.GetReleaseImage(c.instanceGroupName, job.Name)
		if err != nil {
			return []corev1.Container{}, err
		}

		bpmConfig, ok := c.bpmConfigs[job.Name]
		if !ok {
			return nil, errors.Errorf("failed to lookup bpm config for bosh job '%s' in bpm configs", job.Name)
		}
		if len(bpmConfig.Processes) < 1 {
			// we won't create any containers for this job
			continue
		}

		jobDisks := bpmDisks.Filter("job_name", job.Name)
		ephemeralMount, persistentDiskMount := jobDisks.BPMMounts()

		for _, process := range bpmConfig.Processes {
			// extra volume mounts for this container
			processDisks := jobDisks.Filter("process_name", process.Name)
			volumeMounts := deduplicateVolumeMounts(proccessVolumentMounts(defaultVolumeMounts, processDisks, ephemeralMount, persistentDiskMount))

			command := []string{"/usr/bin/dumb-init", "--"}
			args := []string{process.Executable}
			args = append(args, process.Args...)

			// each bpm process gets one container
			container, err := bpmProcessContainer(
				job.Name,
				process.Name,
				jobImage,
				process,
				command,
				args,
				volumeMounts,
				bpmConfig.Run.HealthCheck,
				job.Properties.Quarks.Envs,
				bpmConfig.Run.SecurityContext.DeepCopy(),
			)
			if err != nil {
				return []corev1.Container{}, err
			}

			containers = append(containers, *container.DeepCopy())
		}
	}

	// TODO why give an errand a log sidecar, ever?
	if !c.disableLogSidecar {
		logsTailer := logsTailerContainer()
		containers = append(containers, logsTailer)
	}

	return containers, nil
}

// logsTailerContainer is a container that tails all logs in /var/vcap/sys/log.
func logsTailerContainer() corev1.Container {
	return corev1.Container{
		Name:            "logs",
		Image:           operatorimage.GetOperatorDockerImage(),
		ImagePullPolicy: operatorimage.GetOperatorImagePullPolicy(),
		VolumeMounts:    []corev1.VolumeMount{*sysDirVolumeMount()},
		Args: []string{
			"util",
			"tail-logs",
		},
		Env: []corev1.EnvVar{
			{
				Name:  EnvLogsDir,
				Value: "/var/vcap/sys/log",
			},
			{
				Name:  "LOGROTATE_INTERVAL",
				Value: strconv.Itoa(logrotate.GetInterval()),
			},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser: &rootUserID,
		},
	}
}

// Command represents a command to be run.
type postStartCmd struct {
	Name string
	Arg  []string
}

// postStart controls the --post-start-* feature of the container-run wrapper
type postStart struct {
	command, condition *postStartCmd
}

func bpmProcessContainer(
	jobName string,
	processName string,
	jobImage string,
	process bpm.Process,
	command []string,
	args []string,
	volumeMounts []corev1.VolumeMount,
	healthChecks map[string]bpm.HealthCheck,
	quarksEnvs []corev1.EnvVar,
	securityContext *corev1.SecurityContext,
) (corev1.Container, error) {
	name := names.Sanitize(fmt.Sprintf("%s-%s", jobName, processName))

	if securityContext == nil {
		securityContext = &corev1.SecurityContext{}
	}
	if securityContext.Capabilities == nil && len(process.Capabilities) > 0 {
		securityContext.Capabilities = &corev1.Capabilities{
			Add: capability(process.Capabilities),
		}
	}
	if securityContext.Privileged == nil {
		securityContext.Privileged = &process.Unsafe.Privileged
	}
	if securityContext.RunAsUser == nil {
		securityContext.RunAsUser = &rootUserID
	}

	workdir := process.Workdir
	if workdir == "" {
		workdir = filepath.Join(VolumeJobsDirMountPath, jobName)
	}

	limits := corev1.ResourceList{}
	if process.Limits.Memory != "" {
		quantity, err := resource.ParseQuantity(process.Limits.Memory)
		if err != nil {
			return corev1.Container{}, fmt.Errorf("error parsing memory limit '%s': %v", process.Limits.Memory, err)
		}
		limits[corev1.ResourceMemory] = quantity
	}
	if process.Limits.CPU != "" {
		quantity, err := resource.ParseQuantity(process.Limits.CPU)
		if err != nil {
			return corev1.Container{}, fmt.Errorf("error parsing cpu limit '%s': %v", process.Limits.CPU, err)
		}
		limits[corev1.ResourceCPU] = quantity
	}

	newEnvs := process.NewEnvs(quarksEnvs)
	newEnvs = defaultEnv(newEnvs, map[string]corev1.EnvVar{
		EnvPodOrdinal: podOrdinalEnv,
		EnvReplicas:   replicasEnv,
		EnvAzIndex:    azIndexEnv,
	})

	container := corev1.Container{
		Name:            names.Sanitize(name),
		Image:           jobImage,
		VolumeMounts:    volumeMounts,
		Command:         command,
		Args:            args,
		Env:             newEnvs,
		Lifecycle:       &corev1.Lifecycle{},
		WorkingDir:      workdir,
		SecurityContext: securityContext,
		Resources: corev1.ResourceRequirements{
			Requests: process.Requests,
			Limits:   limits,
		},
	}

	for name, hc := range healthChecks {
		if name == process.Name {
			if hc.ReadinessProbe != nil {
				container.ReadinessProbe = hc.ReadinessProbe
			}
			if hc.LivenessProbe != nil {
				container.LivenessProbe = hc.LivenessProbe
			}
		}
	}
	return container, nil
}

// defaultEnv adds the default value if no value is set
func defaultEnv(envs []corev1.EnvVar, defaults map[string]corev1.EnvVar) []corev1.EnvVar {
	for _, env := range envs {
		delete(defaults, env.Name)
	}

	for _, env := range defaults {
		envs = append(envs, env)
	}
	return envs
}

// generateBPMCommand generates the bpm container arguments.
func generateBPMCommand(
	jobName string,
	process *bpm.Process,
	postStart postStart,
) ([]string, []string) {
	command := []string{"/usr/bin/dumb-init", "--"}
	args := []string{fmt.Sprintf("%s/container-run/container-run", VolumeRenderingDataMountPath)}
	if postStart.command != nil {
		args = append(args, "--post-start-name", postStart.command.Name)
		if postStart.condition != nil {
			args = append(args, "--post-start-condition-name", postStart.condition.Name)
			for _, arg := range postStart.condition.Arg {
				args = append(args, "--post-start-condition-arg", arg)
			}
		}
	}
	args = append(args, "--job-name", jobName)
	args = append(args, "--process-name", process.Name)
	args = append(args, "--")
	args = append(args, process.Executable)
	args = append(args, process.Args...)

	return command, args
}

func newDrainScript(jobName string, processCount string) *corev1.LifecycleHandler {
	drainScript := filepath.Join(VolumeJobsDirMountPath, jobName, "bin", "drain")
	return &corev1.LifecycleHandler{
		Exec: &corev1.ExecAction{
			Command: []string{
				"/bin/sh",
				"-c",
				`
shopt -s nullglob
waitExit() {
	e="$1"
	touch /mnt/drain-stamps/` + jobName + `
	echo "Waiting for other drain scripts to finish."
	while [ $(ls -1 /mnt/drain-stamps | wc -l) -lt ` + processCount + ` ]; do sleep 5; done
	exit "$e"
}
s="` + drainScript + `"
if [ ! -x "$s" ]; then
	waitExit 0
fi
echo "Running drain script $s for ` + jobName + `"
while true; do
	out=$( $s )
	status=$?

	if [ "$status" -ne "0" ]; then
		echo "$s FAILED with exit code $status"
		waitExit $status
	fi

	if [ "$out" -lt "0" ]; then
		echo "Sleeping dynamic draining wait time for $s..."
		sleep ${out:1}
		echo "Running $s again"
	else
		echo "Sleeping static draining wait time for $s..."
		sleep $out
		echo "$s done"
		waitExit 0
	fi
done
echo "Done"`,
			},
		},
	}
}

func newDrainWait(processCount string) *corev1.LifecycleHandler {
	return &corev1.LifecycleHandler{
		Exec: &corev1.ExecAction{
			Command: []string{
				"/bin/sh",
				"-c",
				`
echo "Wait for drain scripts in other containers to finish"
while [ $(ls -1 /mnt/drain-stamps | wc -l) -lt ` + processCount + ` ]; do sleep 5; done
exit 0
echo "Done"
`,
			},
		},
	}
}
