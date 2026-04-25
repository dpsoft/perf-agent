package pprof

import (
	"bytes"
	"testing"

	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// === EXISTING FUNCTIONALITY TESTS ===

func TestNewProfileBuilders(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{
		SampleRate:    99,
		PerPIDProfile: false,
	})

	assert.NotNil(t, builders.Builders)
	assert.Empty(t, builders.Builders)
}

func TestSampleTypes(t *testing.T) {
	tests := []struct {
		name       string
		sampleType SampleType
		wantType   string
		wantUnit   string
	}{
		{"CPU", SampleTypeCpu, "cpu", "nanoseconds"},
		{"OffCPU", SampleTypeOffCpu, "offcpu", "nanoseconds"},
		{"Memory", SampleTypeMem, "alloc_objects", "count"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builders := NewProfileBuilders(BuildersOptions{SampleRate: 99})
			builders.AddSample(&ProfileSample{
				SampleType: tt.sampleType,
				Stack:      FramesFromNames([]string{"main"}),
				Value:      100,
			})

			for _, b := range builders.Builders {
				assert.Equal(t, tt.wantType, b.Profile.SampleType[0].Type)
				assert.Equal(t, tt.wantUnit, b.Profile.SampleType[0].Unit)
			}
		})
	}
}

func TestAddSampleCreatesLocationsAndFunctions(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{SampleRate: 99})

	builders.AddSample(&ProfileSample{
		SampleType:  SampleTypeCpu,
		Aggregation: SampleAggregated,
		Stack:       FramesFromNames([]string{"main", "foo", "bar"}),
		Value:       100,
	})

	for _, b := range builders.Builders {
		assert.Len(t, b.Profile.Location, 3)
		assert.Len(t, b.Profile.Function, 3)
		assert.Len(t, b.Profile.Sample, 1)

		funcNames := make([]string, len(b.Profile.Function))
		for i, f := range b.Profile.Function {
			funcNames[i] = f.Name
		}
		assert.Contains(t, funcNames, "main")
		assert.Contains(t, funcNames, "foo")
		assert.Contains(t, funcNames, "bar")
	}
}

func TestSampleAggregation(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{SampleRate: 99})

	for i := 0; i < 3; i++ {
		builders.AddSample(&ProfileSample{
			SampleType:  SampleTypeCpu,
			Aggregation: false,
			Stack:       FramesFromNames([]string{"main", "handler"}),
			Value:       100,
		})
	}

	for _, b := range builders.Builders {
		assert.Len(t, b.Profile.Sample, 1)
		assert.Equal(t, int64(300*b.Profile.Period), b.Profile.Sample[0].Value[0])
	}
}

func TestPerPIDProfile(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{
		SampleRate:    99,
		PerPIDProfile: true,
	})

	builders.AddSample(&ProfileSample{
		Pid:        1000,
		SampleType: SampleTypeCpu,
		Stack:      FramesFromNames([]string{"main"}),
		Value:      100,
	})
	builders.AddSample(&ProfileSample{
		Pid:        2000,
		SampleType: SampleTypeCpu,
		Stack:      FramesFromNames([]string{"worker"}),
		Value:      200,
	})

	assert.Len(t, builders.Builders, 2)
}

func TestProfileWriteAndParse(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{SampleRate: 99})

	builders.AddSample(&ProfileSample{
		SampleType:  SampleTypeCpu,
		Aggregation: SampleAggregated,
		Stack:       FramesFromNames([]string{"main", "processRequest", "doWork"}),
		Value:       500,
	})

	var buf bytes.Buffer
	for _, b := range builders.Builders {
		_, err := b.Write(&buf)
		require.NoError(t, err)
	}

	parsed, err := profile.Parse(&buf)
	require.NoError(t, err)

	assert.Len(t, parsed.Sample, 1)
	assert.Len(t, parsed.Location, 3)
	assert.Len(t, parsed.Function, 3)
	assert.Equal(t, "cpu", parsed.SampleType[0].Type)
}

func TestOffCpuValueNotScaled(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{SampleRate: 99})

	builders.AddSample(&ProfileSample{
		SampleType:  SampleTypeOffCpu,
		Aggregation: SampleAggregated,
		Stack:       FramesFromNames([]string{"main"}),
		Value:       1000000,
	})

	for _, b := range builders.Builders {
		assert.Equal(t, int64(1000000), b.Profile.Sample[0].Value[0])
	}
}

// === COMMENTS/TAGS TESTS ===

func TestProfileComments(t *testing.T) {
	comments := []string{"env=prod", "version=1.2.3", "service=api"}

	builders := NewProfileBuilders(BuildersOptions{
		SampleRate: 99,
		Comments:   comments,
	})

	builders.AddSample(&ProfileSample{
		SampleType: SampleTypeCpu,
		Stack:      FramesFromNames([]string{"main"}),
		Value:      100,
	})

	for _, b := range builders.Builders {
		assert.Equal(t, comments, b.Profile.Comments)
	}
}

func TestProfileCommentsEmpty(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{
		SampleRate: 99,
		Comments:   nil,
	})

	builders.AddSample(&ProfileSample{
		SampleType: SampleTypeCpu,
		Stack:      FramesFromNames([]string{"main"}),
		Value:      100,
	})

	for _, b := range builders.Builders {
		assert.Empty(t, b.Profile.Comments)
	}
}

func TestProfileWriteWithComments(t *testing.T) {
	comments := []string{"env=staging", "commit=abc123"}

	builders := NewProfileBuilders(BuildersOptions{
		SampleRate: 99,
		Comments:   comments,
	})

	builders.AddSample(&ProfileSample{
		SampleType: SampleTypeCpu,
		Stack:      FramesFromNames([]string{"main"}),
		Value:      100,
	})

	var buf bytes.Buffer
	for _, b := range builders.Builders {
		_, err := b.Write(&buf)
		require.NoError(t, err)
	}

	parsed, err := profile.Parse(&buf)
	require.NoError(t, err)
	assert.Equal(t, comments, parsed.Comments)
}

// === PERF-MAP FRAME DECODER TESTS ===

func TestDecodePerfMapFrame_Python(t *testing.T) {
	tests := []struct {
		name     string
		in       Frame
		wantName string
		wantFile string
	}{
		{
			name:     "stdlib function",
			in:       Frame{Name: "py::Queue.put:/usr/lib/python3.12/queue.py"},
			wantName: "py::Queue.put",
			wantFile: "/usr/lib/python3.12/queue.py",
		},
		{
			name:     "nested class method",
			in:       Frame{Name: "py::Condition.__enter__:/usr/lib/python3.12/threading.py"},
			wantName: "py::Condition.__enter__",
			wantFile: "/usr/lib/python3.12/threading.py",
		},
		{
			name:     "site-packages deep path",
			in:       Frame{Name: "py::HttpClient.send_request:/app/python3.12/site-packages/newrelic/common/agent_http.py"},
			wantName: "py::HttpClient.send_request",
			wantFile: "/app/python3.12/site-packages/newrelic/common/agent_http.py",
		},
		{
			name:     "locals qualifier",
			in:       Frame{Name: "py::parse_url.<locals>.ensure_type:/app/python3.12/site-packages/newrelic/packages/urllib3/util/url.py"},
			wantName: "py::parse_url.<locals>.ensure_type",
			wantFile: "/app/python3.12/site-packages/newrelic/packages/urllib3/util/url.py",
		},
		{
			name:     "prefix only, no path",
			in:       Frame{Name: "py::something_without_path"},
			wantName: "py::something_without_path",
			wantFile: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodePerfMapFrame(tt.in)
			assert.Equal(t, tt.wantName, got.Name)
			assert.Equal(t, tt.wantFile, got.File)
		})
	}
}

func TestDecodePerfMapFrame_Node(t *testing.T) {
	tests := []struct {
		name     string
		in       Frame
		wantName string
		wantFile string
		wantLine uint32
	}{
		{
			name:     "LazyCompile with line and column",
			in:       Frame{Name: "LazyCompile:~cpuWork /tmp/nodeapp.js:4:18"},
			wantName: "LazyCompile:~cpuWork",
			wantFile: "/tmp/nodeapp.js",
			wantLine: 4,
		},
		{
			name:     "JS optimized tier (modern Node v16+)",
			in:       Frame{Name: "JS:*cpuWork /home/user/app.js:12:17"},
			wantName: "JS:*cpuWork",
			wantFile: "/home/user/app.js",
			wantLine: 12,
		},
		{
			name:     "JS lazy tier",
			in:       Frame{Name: "JS:~handler /app/server.js:42:1"},
			wantName: "JS:~handler",
			wantFile: "/app/server.js",
			wantLine: 42,
		},
		{
			name:     "JS sparkplug tier",
			in:       Frame{Name: "JS:^render /app/view.js:7"},
			wantName: "JS:^render",
			wantFile: "/app/view.js",
			wantLine: 7,
		},
		{
			name:     "Function with line only",
			in:       Frame{Name: "Function:handler /app/server.js:42"},
			wantName: "Function:handler",
			wantFile: "/app/server.js",
			wantLine: 42,
		},
		{
			name:     "Builtin with no file info",
			in:       Frame{Name: "Builtin:ArrayForEach"},
			wantName: "Builtin:ArrayForEach",
			wantFile: "",
			wantLine: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodePerfMapFrame(tt.in)
			assert.Equal(t, tt.wantName, got.Name)
			assert.Equal(t, tt.wantFile, got.File)
			assert.Equal(t, tt.wantLine, got.Line)
		})
	}
}

func TestDecodePerfMapFrame_PassThrough(t *testing.T) {
	// Native frames (DWARF-resolved) already have File populated — the
	// decoder must leave them untouched.
	native := Frame{Name: "main.processRequest", File: "/app/main.go", Line: 42}
	assert.Equal(t, native, decodePerfMapFrame(native))

	// Unknown runtime (Java-style) returned as-is.
	java := Frame{Name: "HashMap::get"}
	assert.Equal(t, java, decodePerfMapFrame(java))

	// Plain native symbol without file returns unchanged.
	plain := Frame{Name: "memcpy"}
	assert.Equal(t, plain, decodePerfMapFrame(plain))
}

func TestAddSamplePopulatesFilenameFromPythonPerfMap(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{SampleRate: 99})
	builders.AddSample(&ProfileSample{
		SampleType:  SampleTypeCpu,
		Aggregation: SampleAggregated,
		Stack: []Frame{
			{Name: "py::Queue.put:/usr/lib/python3.12/queue.py"},
			{Name: "main.handler", File: "/app/main.go", Line: 12},
		},
		Value: 100,
	})
	for _, b := range builders.Builders {
		byName := make(map[string]*profile.Function, len(b.Profile.Function))
		for _, f := range b.Profile.Function {
			byName[f.Name] = f
		}
		pyFunc, ok := byName["py::Queue.put"]
		require.True(t, ok, "python function should be indexed by decoded name")
		assert.Equal(t, "/usr/lib/python3.12/queue.py", pyFunc.Filename)

		goFunc, ok := byName["main.handler"]
		require.True(t, ok)
		assert.Equal(t, "/app/main.go", goFunc.Filename)
	}
}

func TestAddSamplePreservesLineFromFrame(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{SampleRate: 99})
	builders.AddSample(&ProfileSample{
		SampleType:  SampleTypeCpu,
		Aggregation: SampleAggregated,
		Stack: []Frame{
			{Name: "main.handler", File: "/app/main.go", Line: 42},
		},
		Value: 100,
	})
	for _, b := range builders.Builders {
		require.Len(t, b.Profile.Location, 1)
		loc := b.Profile.Location[0]
		require.Len(t, loc.Line, 1)
		assert.Equal(t, int64(42), loc.Line[0].Line)
	}
}

// Regression test for the frameKey-excludes-Line bug: two samples in the
// same function at different source lines must produce two distinct
// Locations (but share a single Function).
func TestAddSameFunctionDifferentLinesMakesDistinctLocations(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{SampleRate: 99})
	builders.AddSample(&ProfileSample{
		SampleType:  SampleTypeCpu,
		Aggregation: SampleAggregated,
		Stack:       []Frame{{Name: "main.handler", File: "/app/main.go", Line: 10}},
		Value:       100,
	})
	builders.AddSample(&ProfileSample{
		SampleType:  SampleTypeCpu,
		Aggregation: SampleAggregated,
		Stack:       []Frame{{Name: "main.handler", File: "/app/main.go", Line: 42}},
		Value:       100,
	})
	for _, b := range builders.Builders {
		assert.Len(t, b.Profile.Location, 2, "different lines should produce distinct locations")
		assert.Len(t, b.Profile.Function, 1, "same function should dedupe")

		var lines []int64
		for _, loc := range b.Profile.Location {
			require.Len(t, loc.Line, 1)
			lines = append(lines, loc.Line[0].Line)
		}
		assert.ElementsMatch(t, []int64{10, 42}, lines)
	}
}

// Same function name from different modules must not collapse.
func TestAddSameNameDifferentModulesMakesDistinctFunctions(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{SampleRate: 99})
	builders.AddSample(&ProfileSample{
		SampleType:  SampleTypeCpu,
		Aggregation: SampleAggregated,
		Stack:       []Frame{{Name: "compress", Module: "/usr/lib/libz.so.1"}},
		Value:       100,
	})
	builders.AddSample(&ProfileSample{
		SampleType:  SampleTypeCpu,
		Aggregation: SampleAggregated,
		Stack:       []Frame{{Name: "compress", Module: "/opt/custom/libfoo.so"}},
		Value:       100,
	})
	for _, b := range builders.Builders {
		assert.Len(t, b.Profile.Function, 2, "same name in different modules must not collapse")
	}
}
