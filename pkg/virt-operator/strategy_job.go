package virt_operator

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/virt-operator/resource/apply"
	"kubevirt.io/kubevirt/pkg/virt-operator/resource/generate/components"
	"kubevirt.io/kubevirt/pkg/virt-operator/util"
	operatorutil "kubevirt.io/kubevirt/pkg/virt-operator/util"
)

const (
	// Attempts of the job itself. The operator recreates the job and backs off
	// between attempts, so there is no need for a long in-job backoff on top.
	installStrategyJobBackoffLimit = 3
	// Wall clock budget of the job, covering scheduling and every attempt. Kept
	// generous because a cold node pulling the operator image over a slow
	// registry must not be killed mid-pull; reaching a terminal state at all is
	// what matters, and the retry cadence is set by the operator.
	installStrategyJobDeadlineSeconds = 1800
	// Backstop for a job the operator never gets to collect, for instance
	// because it is not running. A job it does see is removed much earlier.
	installStrategyJobTTLSeconds = 3600
)

func (c *KubeVirtController) generateInstallStrategyJob(infraPlacement *v1.ComponentConfig, config *operatorutil.KubeVirtDeploymentConfig) (*batchv1.Job, error) {

	operatorImage := config.VirtOperatorImage
	if operatorImage == "" {
		operatorImage = fmt.Sprintf("%s/%s%s%s", config.GetImageRegistry(), config.GetImagePrefix(), VirtOperator, components.AddVersionSeparatorPrefix(config.GetOperatorVersion()))
	}
	deploymentConfigJson, err := config.GetJson()
	if err != nil {
		return nil, err
	}

	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "batch/v1",
			Kind:       "Job",
		},

		ObjectMeta: metav1.ObjectMeta{
			Namespace:    c.operatorNamespace,
			GenerateName: fmt.Sprintf("kubevirt-%s-job", config.GetDeploymentID()),
			Labels: map[string]string{
				v1.AppLabel:             "",
				v1.ManagedByLabel:       v1.ManagedByLabelOperatorValue,
				v1.InstallStrategyLabel: "",
			},
			Annotations: map[string]string{
				// Deprecated, keep it for backwards compatibility
				v1.InstallStrategyVersionAnnotation: config.GetKubeVirtVersion(),
				// Deprecated, keep it for backwards compatibility
				v1.InstallStrategyRegistryAnnotation:   config.GetImageRegistry(),
				v1.InstallStrategyIdentifierAnnotation: config.GetDeploymentID(),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: pointer.Int32(installStrategyJobBackoffLimit),
			// The job's pod can stay Pending indefinitely (e.g. unschedulable
			// or broken CNI), which backoffLimit never catches; the deadline
			// forces a terminal JobFailed condition so the job gets recreated.
			// Keep it generous: a cold node pulling the operator image over a
			// slow registry must not be killed mid-pull, and the retry cadence
			// is driven by the operator, not by this deadline.
			ActiveDeadlineSeconds: pointer.Int64(installStrategyJobDeadlineSeconds),
			// Backstop cleanup: if the operator misses the finished job, TTL
			// removes it and a fresh one is created on the next sync.
			TTLSecondsAfterFinished: pointer.Int32(installStrategyJobTTLSeconds),
			Template: k8sv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.AppLabel: virtOperatorJobAppLabel,
					},
				},
				Spec: k8sv1.PodSpec{
					ServiceAccountName: "kubevirt-operator",
					RestartPolicy:      k8sv1.RestartPolicyNever,
					ImagePullSecrets:   config.GetImagePullSecrets(),
					Tolerations:        []k8sv1.Toleration{{Operator: k8sv1.TolerationOpExists}},
					Affinity: &k8sv1.Affinity{PodAffinity: &k8sv1.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []k8sv1.PodAffinityTerm{{
							TopologyKey: "kubernetes.io/hostname",
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{{
									Key:      v1.AppLabel,
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{VirtOperator},
								}},
							},
						}},
					}},

					Containers: []k8sv1.Container{
						{
							Name:            "install-strategy-upload",
							Image:           operatorImage,
							ImagePullPolicy: config.GetImagePullPolicy(),
							Command: []string{
								VirtOperator,
								"--dump-install-strategy",
							},
							Env: []k8sv1.EnvVar{
								{
									Name:  util.VirtOperatorImageEnvName,
									Value: operatorImage,
								},
								{
									// Deprecated, keep it for backwards compatibility
									Name:  util.TargetInstallNamespace,
									Value: config.GetNamespace(),
								},
								{
									// Deprecated, keep it for backwards compatibility
									Name:  util.TargetImagePullPolicy,
									Value: string(config.GetImagePullPolicy()),
								},
								{
									Name:  util.TargetDeploymentConfig,
									Value: deploymentConfigJson,
								},
							},
							SecurityContext: &k8sv1.SecurityContext{
								ReadOnlyRootFilesystem: pointer.Bool(true),
							},
						},
					},
				},
			},
		},
	}

	apply.InjectPlacementMetadata(infraPlacement, &job.Spec.Template.Spec, apply.AnyNode)
	env := job.Spec.Template.Spec.Containers[0].Env
	extraEnv := util.NewEnvVarMap(config.GetExtraEnv())
	job.Spec.Template.Spec.Containers[0].Env = append(env, *extraEnv...)

	return job, nil
}

// installStrategyJobFinishedAt returns the time the install strategy job
// terminated, whether it completed or failed, or nil if it is still running.
// A failed job never gets a CompletionTime, so the JobFailed condition has to
// be consulted as well. The second return value describes the failure for
// logging and events, and is empty for a job that completed.
func installStrategyJobFinishedAt(job *batchv1.Job) (finishedAt *metav1.Time, failure string) {
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime, ""
	}
	for i := range job.Status.Conditions {
		condition := job.Status.Conditions[i]
		if condition.Type == batchv1.JobFailed && condition.Status == k8sv1.ConditionTrue {
			return &condition.LastTransitionTime, jobFailureDescription(condition)
		}
	}
	return nil, ""
}

// jobFailureDescription renders the reason of a failed job for logs and events,
// and stays empty when the condition carries nothing to say.
func jobFailureDescription(condition batchv1.JobCondition) string {
	switch {
	case condition.Reason != "" && condition.Message != "":
		return fmt.Sprintf("%s: %s", condition.Reason, condition.Message)
	case condition.Reason != "":
		return condition.Reason
	default:
		return condition.Message
	}
}

func (c *KubeVirtController) getInstallStrategyJob(config *operatorutil.KubeVirtDeploymentConfig) (*batchv1.Job, bool) {
	objs := c.stores.InstallStrategyJobCache.List()
	for _, obj := range objs {
		if job, ok := obj.(*batchv1.Job); ok {
			if job.Annotations == nil {
				continue
			}

			if idAnno, ok := job.Annotations[v1.InstallStrategyIdentifierAnnotation]; ok && idAnno == config.GetDeploymentID() {
				return job, true
			}

		}
	}
	return nil, false
}

func (c *KubeVirtController) garbageCollectInstallStrategyJobs() error {
	batch := c.clientset.BatchV1()
	jobs := c.stores.InstallStrategyJobCache.List()

	for _, obj := range jobs {
		job, ok := obj.(*batchv1.Job)
		if !ok {
			continue
		}
		if finishedAt, _ := installStrategyJobFinishedAt(job); finishedAt == nil {
			continue
		}

		// Background propagation, so that a pod stuck terminating on an
		// unreachable node does not keep the job around and make this run on
		// every sync. The job may also be gone already: its own TTL and this
		// collector race each other.
		propagationPolicy := metav1.DeletePropagationBackground
		err := batch.Jobs(job.Namespace).Delete(context.Background(), job.Name, metav1.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		})
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
		log.Log.Object(job).Infof("Garbage collected finished install strategy job")
	}

	return nil
}
