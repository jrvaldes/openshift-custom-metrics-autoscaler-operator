package transform

import (
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	mf "github.com/manifestival/manifestival"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
)

var (
	logLevelsKedaOperator        = []string{"debug", "info", "error"}
	logEncodersKedaOperator      = []string{"json", "console"}
	logTimeEncodingsKedaOperator = []string{"epoch", "millis", "nano", "iso8601", "rfc3339", "rfc3339nano"}
)

type Prefix string

const (
	LogLevelKedaOperator        Prefix = "--zap-log-level="
	LogEncoderKedaOperator      Prefix = "--zap-encoder="
	LogTimeEncodingKedaOperator Prefix = "--zap-time-encoding="
	LogLevelMetricsServer       Prefix = "--v="
	ClientCAFile                Prefix = "--client-ca-file="
	TLSCertFile                 Prefix = "--tls-cert-file="
	TLSPrivateKeyFile           Prefix = "--tls-private-key-file="
)

func (p Prefix) String() string {
	return string(p)
}

const (
	containerNameKedaOperator  = "keda-operator"
	containerNameMetricsServer = "keda-metrics-apiserver"
)

func ReplaceNamespace(name string, namespace string, scheme *runtime.Scheme, logger logr.Logger) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetName() == name {
			logger.Info("Changing namespace to " + namespace)

			rolebinding := &rbacv1.RoleBinding{}
			if err := scheme.Convert(u, rolebinding, nil); err != nil {
				return err
			}

			rolebinding.Namespace = namespace

			if err := scheme.Convert(rolebinding, u, nil); err != nil {
				return err
			}
		}
		return nil
	}
}

func ReplaceWatchNamespace(watchNamespace string, containerName string, scheme *runtime.Scheme, logger logr.Logger) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		changed := false
		if u.GetKind() == "Deployment" {
			deploy := &appsv1.Deployment{}
			if err := scheme.Convert(u, deploy, nil); err != nil {
				return err
			}
			containers := deploy.Spec.Template.Spec.Containers
			for i, container := range containers {
				if container.Name == containerName {
					for j, env := range container.Env {
						if env.Name == "WATCH_NAMESPACE" {
							if env.Value != watchNamespace {
								logger.Info("Replacing", "deployment", container.Name, "WATCH_NAMESPACE", watchNamespace, "previous", env.Value)
								containers[i].Env[j].Value = watchNamespace
								changed = true
							}
							break
						}
					}
					break
				}
			}
			if changed {
				if err := scheme.Convert(deploy, u, nil); err != nil {
					return err
				}
			}
		}
		return nil
	}
}

func EnsureCertInjectionForAPIService(annotation string, annotationValue string, scheme *runtime.Scheme, logger logr.Logger) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() == "APIService" {
			apiService := &apiregistrationv1.APIService{}
			if err := scheme.Convert(u, apiService, nil); err != nil {
				return err
			}
			metav1.SetMetaDataAnnotation(&apiService.ObjectMeta, annotation, annotationValue)
			apiService.Spec.InsecureSkipTLSVerify = false

			if err := scheme.Convert(apiService, u, nil); err != nil {
				return err
			}
		}
		return nil
	}
}

func EnsureCertInjectionForService(serviceName string, annotation string, annotationValue string, scheme *runtime.Scheme, logger logr.Logger) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() == "Service" && u.GetName() == serviceName {
			annotations := u.GetAnnotations()
			if annotations == nil {
				annotations = make(map[string]string)
			}
			annotations[annotation] = annotationValue
			u.SetAnnotations(annotations)
		}
		return nil
	}
}

func EnsureCertInjectionForDeployment(configMapName string, secretName string, scheme *runtime.Scheme, logger logr.Logger) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() == "Deployment" {
			deploy := &appsv1.Deployment{}
			if err := scheme.Convert(u, deploy, nil); err != nil {
				return err
			}

			// add Volumes referencing certs in ConfigMap and Secret
			cabundleVolume := corev1.Volume{
				Name: "cabundle",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: configMapName,
						},
					},
				},
			}
			certsVolume := corev1.Volume{
				Name: "certs",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: secretName,
					},
				},
			}

			volumes := deploy.Spec.Template.Spec.Volumes
			cabundleVolumeFound := false
			certsVolumeFound := false
			for i := range volumes {
				if volumes[i].Name == "cabundle" {
					volumes[i] = cabundleVolume
					cabundleVolumeFound = true
				}
				if volumes[i].Name == "certs" {
					volumes[i] = certsVolume
					certsVolumeFound = true
				}
			}
			if !cabundleVolumeFound {
				deploy.Spec.Template.Spec.Volumes = append(deploy.Spec.Template.Spec.Volumes, cabundleVolume)
			}
			if !certsVolumeFound {
				deploy.Spec.Template.Spec.Volumes = append(deploy.Spec.Template.Spec.Volumes, certsVolume)
			}

			containers := deploy.Spec.Template.Spec.Containers
			for i := range containers {
				if containers[i].Name == containerNameMetricsServer {
					// mount Volumes referencing certs in ConfigMap and Secret
					cabundleVolumeMount := corev1.VolumeMount{
						Name:      "cabundle",
						MountPath: "/cabundle",
					}

					certsVolumeMount := corev1.VolumeMount{
						Name:      "certs",
						MountPath: "/certs",
					}

					volumeMounts := containers[i].VolumeMounts
					cabundleVolumeMountFound := false
					certsVolumeMountFound := false
					for j := range volumeMounts {
						if volumeMounts[j].Name == "cabundle" {
							volumeMounts[j] = cabundleVolumeMount
							cabundleVolumeMountFound = true
						}
						if volumeMounts[j].Name == "certs" {
							volumeMounts[j] = certsVolumeMount
							certsVolumeMountFound = true
						}
					}
					if !cabundleVolumeMountFound {
						containers[i].VolumeMounts = append(containers[i].VolumeMounts, cabundleVolumeMount)
					}
					if !certsVolumeMountFound {
						containers[i].VolumeMounts = append(containers[i].VolumeMounts, certsVolumeMount)
					}

					break
				}
			}

			if err := scheme.Convert(deploy, u, nil); err != nil {
				return err
			}
		}
		return nil
	}
}

func EnsurePathsToCertsInDeployment(values []string, prefixes []Prefix, scheme *runtime.Scheme, logger logr.Logger) []mf.Transformer {
	transforms := []mf.Transformer{}
	for i := range values {
		transforms = append(transforms, replaceContainerArg(values[i], prefixes[i], containerNameMetricsServer, scheme, logger))
	}
	return transforms
}

func EnsureAuditPolicyConfigMapMountsVolume(configMapName string, scheme *runtime.Scheme, logger logr.Logger) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() == "Deployment" {
			deploy := &appsv1.Deployment{}
			if err := scheme.Convert(u, deploy, nil); err != nil {
				return err
			}

			policyVolume := corev1.Volume{
				Name: "audit-policy",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: configMapName,
						},
					},
				},
			}
			volumes := deploy.Spec.Template.Spec.Volumes
			policyVolumeFound := false
			for i := range volumes {
				if volumes[i].Name == "audit-policy" {
					volumes[i] = policyVolume
					policyVolumeFound = true
				}
			}

			// add Volume to deployment if not found
			if !policyVolumeFound {
				deploy.Spec.Template.Spec.Volumes = append(deploy.Spec.Template.Spec.Volumes, policyVolume)
			}

			containers := deploy.Spec.Template.Spec.Containers
			for i := range containers {
				if containers[i].Name == containerNameMetricsServer {
					policyVolumeMount := corev1.VolumeMount{
						Name:      "audit-policy",
						MountPath: "/var/audit-policy",
					}

					volumeMounts := containers[i].VolumeMounts
					policyVolumeMountFound := false
					for j := range volumeMounts {
						if volumeMounts[j].Name == "audit-policy" {
							volumeMounts[j] = policyVolumeMount
							policyVolumeMountFound = true
						}
					}
					// add VolumeMount to deployment if not found
					if !policyVolumeMountFound {
						containers[i].VolumeMounts = append(containers[i].VolumeMounts, policyVolumeMount)
					}

					break
				}
			}
			if err := scheme.Convert(deploy, u, nil); err != nil {
				return err
			}
		}
		return nil
	}
}

func ReplaceKedaOperatorLogLevel(logLevel string, scheme *runtime.Scheme, logger logr.Logger) mf.Transformer {
	found := false
	for _, level := range logLevelsKedaOperator {
		if logLevel == level {
			found = true
		}
	}
	if !found {
		if _, err := strconv.ParseUint(logLevel, 10, 64); err == nil {
			found = true
		}
	}

	if !found {
		logger.Info("Ignoring speficied Log level for KEDA Operator, it needs to be set to ", strings.Join(logLevelsKedaOperator, ", "), "or an integer value greater than 0")
		return func(u *unstructured.Unstructured) error {
			return nil
		}
	}

	prefix := LogLevelKedaOperator
	return replaceContainerArg(logLevel, prefix, containerNameKedaOperator, scheme, logger)
}

func ReplaceKedaOperatorLogEncoder(logEncoder string, scheme *runtime.Scheme, logger logr.Logger) mf.Transformer {
	found := false
	for _, format := range logEncodersKedaOperator {
		if logEncoder == format {
			found = true
			break
		}
	}

	if !found {
		logger.Info("Ignoring speficied Log encoder for KEDA Operator", "specified", logEncoder, "allowed values", strings.Join(logEncodersKedaOperator, ", "))
		return func(u *unstructured.Unstructured) error {
			return nil
		}
	}

	prefix := LogEncoderKedaOperator
	return replaceContainerArg(logEncoder, prefix, containerNameKedaOperator, scheme, logger)
}

func ReplaceMetricsServerLogLevel(logLevel string, scheme *runtime.Scheme, logger logr.Logger) mf.Transformer {
	found := false
	if _, err := strconv.ParseUint(logLevel, 10, 64); err == nil {
		found = true
	}

	if !found {
		logger.Info("Ignoring speficied Log level for KEDA Metrics Server, it needs to be set to an integer value greater than 0")
		return func(u *unstructured.Unstructured) error {
			return nil
		}
	}

	prefix := LogLevelMetricsServer
	return replaceContainerArg(logLevel, prefix, containerNameMetricsServer, scheme, logger)
}

func ReplaceKedaOperatorLogTimeEncoding(logTimeEncoding string, scheme *runtime.Scheme, logger logr.Logger) mf.Transformer {
	found := false
	for _, timeEncoding := range logTimeEncodingsKedaOperator {
		if logTimeEncoding == timeEncoding {
			found = true
			break
		}
	}

	if !found {
		logger.Info("Ignoring speficied Log time encoding for KEDA Operator", "specified", logTimeEncoding, "allowed values", strings.Join(logTimeEncodingsKedaOperator, ", "))
		return func(u *unstructured.Unstructured) error {
			return nil
		}
	}

	prefix := LogTimeEncodingKedaOperator
	return replaceContainerArg(logTimeEncoding, prefix, containerNameKedaOperator, scheme, logger)
}

func ReplaceArbitraryArg(argument string, resource string, scheme *runtime.Scheme, logger logr.Logger) mf.Transformer {
	prefix := Prefix("")
	prefixStr := ""
	argTrue := ""

	if strings.Contains(argument, "=") {
		// if argument is in format --arg=value or arg=value
		stringSplit := strings.SplitAfter(argument, "=")
		if !strings.HasPrefix(stringSplit[0], "--") {
			// add "--" at the beginning of argument prefix if not given
			prefixStr = "--" + stringSplit[0]
		} else {
			prefixStr = stringSplit[0]
		}

		prefix = Prefix(prefixStr)
		argTrue = stringSplit[1]
	} else {
		// if argument is just value like '/usr/local/bin/keda-adapter' (has no "=" therefore no prefix)
		argTrue = argument
	}

	switch resource {
	case "operator":
		return replaceContainerArg(argTrue, prefix, containerNameKedaOperator, scheme, logger)
	case "metricsserver":
		return replaceContainerArg(argTrue, prefix, containerNameMetricsServer, scheme, logger)
	default:
		return func(u *unstructured.Unstructured) error {
			return nil
		}
	}
}

func ReplaceAuditConfig(argument string, selector string, scheme *runtime.Scheme, logger logr.Logger) mf.Transformer {
	var prefix string
	switch selector {
	case "policyfile":
		prefix = "--audit-policy-file="
	case "logformat":
		prefix = "--audit-log-format="
	case "logpath":
		prefix = "--audit-log-path="
	case "maxage":
		prefix = "--audit-log-maxage="
	case "maxbackup":
		prefix = "--audit-log-maxbackup="
	case "maxsize":
		prefix = "--audit-log-maxsize="
	default:
		return func(u *unstructured.Unstructured) error {
			return nil
		}
	}
	return replaceContainerArg(argument, Prefix(prefix), containerNameMetricsServer, scheme, logger)
}

func replaceContainerArg(value string, prefix Prefix, containerName string, scheme *runtime.Scheme, logger logr.Logger) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		changed := false
		if u.GetKind() == "Deployment" {
			deploy := &appsv1.Deployment{}
			if err := scheme.Convert(u, deploy, nil); err != nil {
				return err
			}
			containers := deploy.Spec.Template.Spec.Containers
			for i, container := range containers {
				if container.Name == containerName {
					argFound := false
					for j, arg := range container.Args {
						if strings.HasPrefix(arg, prefix.String()) {
							// If argument has no prefix...
							if prefix == "" {
								// and is the same -> dont add it again (change argFound=true)...
								if arg == value {
									argFound = true
									break
								}
								// otherwise continue
								continue
							}

							argFound = true
							if trimmedArg := strings.TrimPrefix(arg, prefix.String()); trimmedArg != value {
								logger.Info("Replacing", "deployment", container.Name, "prefix", prefix.String(), "value", value, "previous", trimmedArg)
								containers[i].Args[j] = prefix.String() + value
								changed = true
							}
							break
						}
					}
					if !argFound {
						logger.Info("Adding", "deployment", container.Name, "prefix", prefix.String(), "value", value)
						containers[i].Args = append(containers[i].Args, prefix.String()+value)
						changed = true
					}
					break
				}
			}
			if changed {
				if err := scheme.Convert(deploy, u, nil); err != nil {
					return err
				}
			}
		}
		return nil
	}
}

func AddServiceAccountAnnotations(annotations map[string]string, scheme *runtime.Scheme) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() == "ServiceAccount" {
			sa := &corev1.ServiceAccount{}
			if err := scheme.Convert(u, sa, nil); err != nil {
				return err
			}

			sa.Annotations = updateMap(sa.Annotations, annotations)

			return scheme.Convert(sa, u, nil)
		}
		return nil
	}
}

func AddServiceAccountLabels(labels map[string]string, scheme *runtime.Scheme) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() == "ServiceAccount" {
			sa := &corev1.ServiceAccount{}
			if err := scheme.Convert(u, sa, nil); err != nil {
				return err
			}

			sa.Labels = updateMap(sa.Labels, labels)

			return scheme.Convert(sa, u, nil)
		}
		return nil
	}
}

func AddPodAnnotations(annotations map[string]string, scheme *runtime.Scheme) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() == "Deployment" {
			deploy := &appsv1.Deployment{}
			if err := scheme.Convert(u, deploy, nil); err != nil {
				return err
			}

			deploy.Spec.Template.Annotations = updateMap(deploy.Spec.Template.Annotations, annotations)

			return scheme.Convert(deploy, u, nil)
		}
		return nil
	}
}

func AddPodLabels(labels map[string]string, scheme *runtime.Scheme) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() == "Deployment" {
			deploy := &appsv1.Deployment{}
			if err := scheme.Convert(u, deploy, nil); err != nil {
				return err
			}

			deploy.Spec.Template.ObjectMeta.Labels = updateMap(deploy.Spec.Template.ObjectMeta.Labels, labels)

			return scheme.Convert(deploy, u, nil)
		}
		return nil
	}
}

func AddDeploymentAnnotations(annotations map[string]string, scheme *runtime.Scheme) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() == "Deployment" {
			deploy := &appsv1.Deployment{}
			if err := scheme.Convert(u, deploy, nil); err != nil {
				return err
			}

			deploy.Annotations = updateMap(deploy.Annotations, annotations)

			return scheme.Convert(deploy, u, nil)
		}
		return nil
	}
}

func AddDeploymentLabels(labels map[string]string, scheme *runtime.Scheme) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() == "Deployment" {
			deploy := &appsv1.Deployment{}
			if err := scheme.Convert(u, deploy, nil); err != nil {
				return err
			}

			deploy.Labels = updateMap(deploy.Labels, labels)

			return scheme.Convert(deploy, u, nil)
		}
		return nil
	}
}

func updateMap(mapToUpdate map[string]string, newValues map[string]string) map[string]string {
	if mapToUpdate != nil {
		for k, v := range newValues {
			mapToUpdate[k] = v
		}
		return mapToUpdate
	}

	return newValues
}

func ReplaceNodeSelector(nodeSelector map[string]string, scheme *runtime.Scheme) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() == "Deployment" {
			deploy := &appsv1.Deployment{}
			if err := scheme.Convert(u, deploy, nil); err != nil {
				return err
			}

			deploy.Spec.Template.Spec.NodeSelector = nodeSelector
			return scheme.Convert(deploy, u, nil)
		}
		return nil
	}
}

func ReplaceTolerations(tolerations []corev1.Toleration, scheme *runtime.Scheme) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() == "Deployment" {
			deploy := &appsv1.Deployment{}
			if err := scheme.Convert(u, deploy, nil); err != nil {
				return err
			}

			deploy.Spec.Template.Spec.Tolerations = tolerations
			return scheme.Convert(deploy, u, nil)
		}
		return nil
	}
}

func ReplaceAffinity(affinity *corev1.Affinity, scheme *runtime.Scheme) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() == "Deployment" {
			deploy := &appsv1.Deployment{}
			if err := scheme.Convert(u, deploy, nil); err != nil {
				return err
			}

			deploy.Spec.Template.Spec.Affinity = affinity
			return scheme.Convert(deploy, u, nil)
		}
		return nil
	}
}

func ReplacePriorityClassName(priorityClassName string, scheme *runtime.Scheme) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() == "Deployment" {
			deploy := &appsv1.Deployment{}
			if err := scheme.Convert(u, deploy, nil); err != nil {
				return err
			}

			deploy.Spec.Template.Spec.PriorityClassName = priorityClassName
			return scheme.Convert(deploy, u, nil)
		}
		return nil
	}
}

func ReplaceKedaOperatorResources(resources corev1.ResourceRequirements, scheme *runtime.Scheme) mf.Transformer {
	return replaceResources(resources, containerNameKedaOperator, scheme)
}

func ReplaceMetricsServerResources(resources corev1.ResourceRequirements, scheme *runtime.Scheme) mf.Transformer {
	return replaceResources(resources, containerNameMetricsServer, scheme)
}

func replaceResources(resources corev1.ResourceRequirements, containerName string, scheme *runtime.Scheme) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() == "Deployment" {
			deploy := &appsv1.Deployment{}
			if err := scheme.Convert(u, deploy, nil); err != nil {
				return err
			}

			containers := deploy.Spec.Template.Spec.Containers
			for i, container := range containers {
				if container.Name == containerName {
					containers[i].Resources = resources
					break
				}
			}
			return scheme.Convert(deploy, u, nil)
		}
		return nil
	}
}

func ReplaceMetricsServerImage(image string, scheme *runtime.Scheme) mf.Transformer {
	return replaceContainerImage(image, containerNameMetricsServer, scheme)
}

func ReplaceKedaOperatorImage(image string, scheme *runtime.Scheme) mf.Transformer {
	return replaceContainerImage(image, containerNameKedaOperator, scheme)
}

func replaceContainerImage(image string, containerName string, scheme *runtime.Scheme) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() == "Deployment" {
			deploy := &appsv1.Deployment{}
			if err := scheme.Convert(u, deploy, nil); err != nil {
				return err
			}

			containers := deploy.Spec.Template.Spec.Containers
			for i, container := range containers {
				if container.Name == containerName {
					containers[i].Image = image
					break
				}
			}
			return scheme.Convert(deploy, u, nil)
		}
		return nil
	}
}

func EnsureAuditLogMount(pvc string, path string, scheme *runtime.Scheme, logger logr.Logger) mf.Transformer {
	const logOutputVolumeName = "audit-log"
	return func(u *unstructured.Unstructured) error {
		// ensure mountVolume exists when volume exists
		if u.GetKind() == "Deployment" {
			deploy := &appsv1.Deployment{}
			if err := scheme.Convert(u, deploy, nil); err != nil {
				return err
			}

			logOutVolume := corev1.Volume{
				Name: logOutputVolumeName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: pvc,
					},
				},
			}
			// find volume by name
			volumes := deploy.Spec.Template.Spec.Volumes
			logVolumeFound := false
			for i := range volumes {
				if volumes[i].Name == logOutputVolumeName {
					volumes[i] = logOutVolume
					logVolumeFound = true
				}
			}

			// if not found
			if !logVolumeFound {
				deploy.Spec.Template.Spec.Volumes = append(deploy.Spec.Template.Spec.Volumes, logOutVolume)
			}

			containers := deploy.Spec.Template.Spec.Containers
			for i := range containers {
				if containers[i].Name == containerNameMetricsServer {
					auditVolumeMount := corev1.VolumeMount{
						Name:      logOutputVolumeName,
						MountPath: path,
					}

					volumeMounts := containers[i].VolumeMounts
					auditLogVolumeMountFound := false
					for j := range volumeMounts {
						if volumeMounts[j].Name == logOutputVolumeName {
							volumeMounts[j] = auditVolumeMount
							auditLogVolumeMountFound = true
						}
					}
					// add VolumeMount to deployment if not found
					if !auditLogVolumeMountFound {
						containers[i].VolumeMounts = append(containers[i].VolumeMounts, auditVolumeMount)
					}
					break
				}
			}
			if err := scheme.Convert(deploy, u, nil); err != nil {
				return err
			}
		}
		return nil
	}
}
