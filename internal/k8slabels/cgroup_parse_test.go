package k8slabels

import "testing"

func TestParseV2CgroupPath(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		want   string
		wantOK bool
	}{
		{
			name:   "pure v2 single line",
			input:  "0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234.slice/cri-containerd-abc.scope\n",
			want:   "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234.slice/cri-containerd-abc.scope",
			wantOK: true,
		},
		{
			name: "hybrid v1+v2: only the 0:: line is used",
			input: "12:devices:/kubepods/burstable/pod-abc/container-xyz\n" +
				"0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234.slice/cri-containerd-abc.scope\n",
			want:   "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234.slice/cri-containerd-abc.scope",
			wantOK: true,
		},
		{
			name:   "v1 only: no 0:: line",
			input:  "12:devices:/some/v1/path\n2:cpu,cpuacct:/foo\n",
			want:   "",
			wantOK: false,
		},
		{
			name:   "empty file",
			input:  "",
			want:   "",
			wantOK: false,
		},
		{
			name:   "v2 line with CRLF terminator",
			input:  "0::/kubepods.slice/foo\r\n",
			want:   "/kubepods.slice/foo",
			wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseV2CgroupPath([]byte(tc.input))
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("parseV2CgroupPath() = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestExtractPodUID(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{
			// systemd-style with dashes
			path: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod12345678_1234_1234_1234_123456789abc.slice/cri-containerd-abc.scope",
			want: "12345678-1234-1234-1234-123456789abc",
		},
		{
			// cgroupfs-style without dashes
			path: "/kubepods/burstable/pod12345678-1234-1234-1234-123456789abc/cri-containerd-abc.scope",
			want: "12345678-1234-1234-1234-123456789abc",
		},
		{
			// no kubepods → no UID
			path: "/system.slice/myservice.scope",
			want: "",
		},
		{
			// kubepods but no podXXX segment
			path: "/kubepods.slice/kubepods-besteffort.slice",
			want: "",
		},
		{
			// mixed - and _ separators: not produced by any kubelet, tightened regex rejects it
			path: "/kubepods.slice/.../pod12345678-1234_1234-1234-123456789abc.slice/foo",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := extractPodUID(tc.path)
			if got != tc.want {
				t.Errorf("extractPodUID(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestExtractContainerID(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{
			path: "/kubepods.slice/.../cri-containerd-abc123def456.scope",
			want: "abc123def456",
		},
		{
			path: "/kubepods.slice/.../crio-9f8e7d6c5b4a.scope",
			want: "9f8e7d6c5b4a",
		},
		{
			path: "/kubepods.slice/.../docker-deadbeef.scope",
			want: "deadbeef",
		},
		{
			path: "/kubepods/burstable/pod-abc/abc123def456",
			want: "abc123def456", // raw container-id leaf (cgroupfs driver)
		},
		{
			path: "/kubepods.slice/kubepods-burstable.slice", // no leaf yet
			want: "",
		},
		{
			path: "/kubepods/burstable/pod-abc/abcdef01234", // 11-char hex leaf, below 12-char threshold
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := extractContainerID(tc.path)
			if got != tc.want {
				t.Errorf("extractContainerID(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
