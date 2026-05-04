package k8slabels

import (
	"reflect"
	"testing"
)

func TestDownwardAPIEnv_AllSet(t *testing.T) {
	t.Setenv("POD_NAME", "my-app-7d8f5c-xkz2q")
	t.Setenv("POD_NAMESPACE", "production")
	t.Setenv("CONTAINER_NAME", "my-app")
	got := downwardAPIEnv()
	want := map[string]string{
		"pod_name":       "my-app-7d8f5c-xkz2q",
		"namespace":      "production",
		"container_name": "my-app",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("downwardAPIEnv() = %v, want %v", got, want)
	}
}

func TestDownwardAPIEnv_AllUnset(t *testing.T) {
	t.Setenv("POD_NAME", "")
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("CONTAINER_NAME", "")
	got := downwardAPIEnv()
	if len(got) != 0 {
		t.Errorf("downwardAPIEnv() with all empty = %v, want empty map", got)
	}
}

func TestDownwardAPIEnv_PartialSet(t *testing.T) {
	t.Setenv("POD_NAME", "my-app-xkz2q")
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("CONTAINER_NAME", "my-app")
	got := downwardAPIEnv()
	want := map[string]string{
		"pod_name":       "my-app-xkz2q",
		"container_name": "my-app",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("downwardAPIEnv() = %v, want %v", got, want)
	}
}
