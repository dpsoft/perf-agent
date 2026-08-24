package flamegraph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeProfile serialises p to a gzipped pprof file, the same shape
// perf-agent writes.
func writeProfile(t *testing.T, p *profile.Profile) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profile.pb.gz")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, p.Write(f))
	require.NoError(t, f.Close())
	return path
}

func fn(name string) *profile.Function { return &profile.Function{ID: 0, Name: name} }

// gpuLikeProfile mirrors the structure of the real RTX 3090 profile: two
// unsampled-launch stacks and one 16-frame joined stack carrying unsymbolized
// vendor frames, root-first, one nanosecond value axis, gpu_* labels.
func gpuLikeProfile(t *testing.T) *profile.Profile {
	t.Helper()
	m := &profile.Mapping{ID: 1}
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "gpu", Unit: "nanoseconds"}},
		PeriodType: &profile.ValueType{Type: "gpu", Unit: "nanoseconds"},
		Period:     1,
		Mapping:    []*profile.Mapping{m},
	}
	var id uint64
	mk := func(name string) *profile.Location {
		id++
		f := fn(name)
		f.ID = id
		p.Function = append(p.Function, f)
		l := &profile.Location{ID: id, Mapping: m, Line: []profile.Line{{Function: f}}}
		p.Location = append(p.Location, l)
		return l
	}
	unsampled := mk("[gpu:launch unsampled]")
	axpy := mk("[gpu:kernel:_Z14perfagent_axpyfPKfPfi]")
	scale := mk("[gpu:kernel:_Z15perfagent_scalePffi]")

	var joined []*profile.Location
	for _, name := range []string{
		"_start", "__libc_start_main_alias_1", "__libc_start_call_main", "main",
		"__device_stub__Z14perfagent_axpyfPKfPfi(float, float const*, float*, int)",
		"cudaLaunchKernel",
		"0x7f2c958b71c6", "0x7f2c945ace62", "0x7f2c945acc75", "0x7f2c945b2dfb",
		"0x7f2c945b2c2b", "0x7f2c945bbf6f", "0x7f2c944de06b",
		"(anonymous namespace)::on_callback(void*, CUpti_CallbackDomain, unsigned int, void const*)",
		"[gpu:launch]",
	} {
		joined = append(joined, mk(name))
	}
	joined = append(joined, axpy)

	label := func(join, corr string) map[string][]string {
		return map[string][]string{"gpu_join": {join}, "gpu_correlation": {corr}}
	}
	p.Sample = []*profile.Sample{
		{Location: []*profile.Location{unsampled, axpy}, Value: []int64{2695822}, Label: label("exact", "cupti:1")},
		{Location: []*profile.Location{unsampled, scale}, Value: []int64{2894901}, Label: label("exact", "cupti:2")},
		{Location: joined, Value: []int64{397131}, Label: label("exact", "cupti:3")},
	}
	return p
}

func TestFromProfileFileRendersARealGPUShapedProfile(t *testing.T) {
	src := writeProfile(t, gpuLikeProfile(t))
	dst := filepath.Join(t.TempDir(), "out.html")

	res, err := FromProfileFile(src, dst, Options{})
	require.NoError(t, err)

	assert.Equal(t, int64(5987854), res.Total, "the folded total must equal the profile's own sum")
	assert.Equal(t, 3, res.Samples)
	assert.Len(t, res.Stacks, 3)
	assert.Equal(t, "gpu", res.SampleTypeName)
	assert.Equal(t, "nanoseconds", res.Unit)
	assert.Equal(t, 16, res.MaxDepth)
	assert.Equal(t, 20, res.Frames) // 2 + 2 + 16
	assert.Equal(t, 7, res.AddressOnlyFrames)
	assert.Zero(t, res.InexactTotal, "every join in this profile is exact")

	html, err := os.ReadFile(dst)
	require.NoError(t, err)
	page := string(html)
	assert.Contains(t, page, "5.99 ms")
	assert.Contains(t, page, "[gpu:kernel:_Z14perfagent_axpyfPKfPfi]")
	assert.Contains(t, page, `data-domain="boundary-unattributed"`)
	assert.Contains(t, page, `data-domain="shim"`)
	assert.Contains(t, page, "gpu_correlation")
	assert.Contains(t, page, "profile.pb.gz", "the page names the file it was rendered from")
}

func TestFromProfileFileWritesAPageForAnEmptyProfile(t *testing.T) {
	// The deliberately empty case: a profile with a declared type and no
	// samples at all must still produce a file, and that file must say why
	// it is blank.
	src := writeProfile(t, &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		PeriodType: &profile.ValueType{Type: "cpu", Unit: "nanoseconds"},
		Period:     1,
	})
	dst := filepath.Join(t.TempDir(), "empty.html")

	res, err := FromProfileFile(src, dst, Options{})
	require.NoError(t, err, "an empty profile is not a rendering failure")
	assert.True(t, res.Degenerate())

	page, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Contains(t, string(page), "No flame graph was drawn")
	assert.Contains(t, string(page), "zero samples")
	assert.NotContains(t, string(page), "<svg class=\"flame\"")
}

func TestFromProfileFileReportsAMissingOrCorruptInput(t *testing.T) {
	_, err := FromProfileFile(filepath.Join(t.TempDir(), "nope.pb.gz"), filepath.Join(t.TempDir(), "o.html"), Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open profile")

	bad := filepath.Join(t.TempDir(), "bad.pb.gz")
	require.NoError(t, os.WriteFile(bad, []byte("not a profile"), 0o600))
	_, err = FromProfileFile(bad, filepath.Join(t.TempDir(), "o.html"), Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse profile")
}

func TestFromProfileFileTitleAndSubtitleDefaultToProvenance(t *testing.T) {
	src := writeProfile(t, gpuLikeProfile(t))
	dst := filepath.Join(t.TempDir(), "out.html")
	_, err := FromProfileFile(src, dst, Options{})
	require.NoError(t, err)

	page, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Contains(t, string(page), "<title>profile.pb.gz</title>")
	assert.True(t, strings.Contains(string(page), src), "the subtitle names the source path")
}
