package k8slabels

import "os"

// downwardAPIEnv returns labels read from canonical Kubernetes downward-API
// environment variables. Unset or empty variables are silently skipped, so
// callers can apply this function on any host without producing spurious
// empty-string labels.
func downwardAPIEnv() map[string]string {
	out := make(map[string]string, 3)
	for envName, labelKey := range map[string]string{
		"POD_NAME":       "pod_name",
		"POD_NAMESPACE":  "namespace",
		"CONTAINER_NAME": "container_name",
	} {
		if v := os.Getenv(envName); v != "" {
			out[labelKey] = v
		}
	}
	return out
}
